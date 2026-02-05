package utils

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"feedora/globals"
	"feedora/models"
	"sync"
	"time"
)

const (
	// 保存间隔（秒）- 用于定期同步内存到数据库
	SaveInterval = 60
	// 清理间隔（小时）
	CleanupInterval = 6
)

var (
	// 持久化数据目录
	DataDir = getDataDir()
	
	// PostProcessCache 后处理结果缓存（内存）
	PostProcessCache     map[string]models.PostProcessCacheEntry
	PostProcessCacheLock sync.RWMutex
	
	// 标记是否有未保存的更改
	dataChanged     bool
	dataChangedLock sync.Mutex
)

// getDataDir 获取数据目录，优先使用环境变量，否则使用./data
func getDataDir() string {
	if dir := os.Getenv("DATA_DIR"); dir != "" {
		return dir
	}
	// Docker环境
	if _, err := os.Stat("/app"); err == nil {
		return "/app/data"
	}
	// 本地开发环境
	return "./data"
}

// ensureDataDir 确保数据目录存在
func ensureDataDir() {
	if _, err := os.Stat(DataDir); os.IsNotExist(err) {
		if err := os.MkdirAll(DataDir, 0755); err != nil {
			log.Printf("创建数据目录失败: %v", err)
		}
	}
}

// InitPersistence 初始化持久化模块
func InitPersistence() {
	PostProcessCache = make(map[string]models.PostProcessCacheEntry)
	
	// 确保数据目录存在
	ensureDataDir()
	
	// 初始化数据库
	if err := InitDatabase(); err != nil {
		log.Printf("[持久化] 数据库初始化失败: %v", err)
		panic(err)
	}
	
	// 加载已保存的数据
	loadPersistedData()
	
	// 启动定期保存任务
	go autoSaveLoop()
	
	// 启动定期清理任务
	go autoCleanupLoop()
}

// loadPersistedData 加载持久化的数据
func loadPersistedData() {
	// 加载分类缓存
	loadClassifyCache()
	// 加载已读状态
	loadReadState()
	// 加载后处理缓存
	loadPostProcessCache()
	// 加载条目缓存
	loadItemsCache()
}

// loadClassifyCache 加载分类缓存
func loadClassifyCache() {
	cache, err := DBLoadClassifyCache()
	if err != nil {
		log.Printf("读取分类缓存失败: %v", err)
		return
	}
	
	globals.ClassifyCacheLock.Lock()
	globals.ClassifyCache = make(map[string]models.ClassifyCacheEntry)
	for link, category := range cache {
		globals.ClassifyCache[link] = models.ClassifyCacheEntry{Category: category}
	}
	globals.ClassifyCacheLock.Unlock()
	
	log.Printf("[数据加载] 分类缓存: 已加载 %d 条", len(cache))
}

// loadReadState 加载已读状态
func loadReadState() {
	state, err := DBLoadReadState()
	if err != nil {
		log.Printf("读取已读状态失败: %v", err)
		return
	}
	
	globals.ReadStateLock.Lock()
	globals.ReadState = state
	globals.ReadStateLock.Unlock()
	
	log.Printf("[数据加载] 已读状态: 已加载 %d 条", len(state))

	// 启动时延迟执行清理，防止离线期间配置变更导致的数据冗余
	go func() {
		for i := 0; i < 12; i++ { // 最多尝试 1 分钟
			time.Sleep(5 * time.Second)
			if isDbMapReady() {
				CleanupPostProcessCacheOnConfigChange()
				CleanupReadStateOnConfigChange()
				CleanupItemsCacheOnConfigChange()
				return
			}
		}
	}()
}

// loadPostProcessCache 加载后处理缓存
func loadPostProcessCache() {
	cache, err := DBLoadPostProcessCache()
	if err != nil {
		log.Printf("读取后处理缓存失败: %v", err)
		return
	}
	
	PostProcessCacheLock.Lock()
	PostProcessCache = make(map[string]models.PostProcessCacheEntry)
	for link, entry := range cache {
		PostProcessCache[link] = models.PostProcessCacheEntry{
			Title:       entry.Title,
			Link:        entry.NewLink,
			PubDate:     entry.PubDate,
			ProcessedAt: entry.ProcessedAt,
		}
	}
	PostProcessCacheLock.Unlock()
	
	log.Printf("[数据加载] 后处理缓存: 已加载 %d 条", len(cache))
}

// loadItemsCache 加载条目缓存
func loadItemsCache() {
	cache, err := DBLoadItemsCache()
	if err != nil {
		log.Printf("读取条目缓存失败: %v", err)
		return
	}
	
	globals.ItemsCacheLock.Lock()
	globals.ItemsCache = make(map[string][]models.Item)
	for rssURL, entries := range cache {
		items := make([]models.Item, len(entries))
		for i, entry := range entries {
			items[i] = models.Item{
				Title:        entry.Title,
				Link:         entry.Link,
				OriginalLink: entry.OriginalLink,
				PubDate:      entry.PubDate,
			}
			// 从分类缓存中恢复类别，这对于文件夹过滤功能至关重要
			globals.ClassifyCacheLock.RLock()
			if cat, ok := globals.ClassifyCache[entry.Link]; ok {
				items[i].Category = cat.Category
			} else if entry.OriginalLink != "" {
				if cat, ok := globals.ClassifyCache[entry.OriginalLink]; ok {
					items[i].Category = cat.Category
				}
			}
			globals.ClassifyCacheLock.RUnlock()
		}
		globals.ItemsCache[rssURL] = items
	}
	globals.ItemsCacheLock.Unlock()
	
	// 同时也填充 DbMap 以便重启后能立即展示缓存
	globals.Lock.Lock()
	for rssURL, items := range globals.ItemsCache {
		source := globals.RssUrls.GetSourceByURL(rssURL)
		title := rssURL
		icon := ""
		showPubDate := false
		showCategory := false
		if source != nil {
			if source.Name != "" {
				title = source.Name
			}
			icon = GetIconForURL(rssURL)
			showPubDate = source.ShowPubDate
			showCategory = source.ShowCategory
		}
		
		// 构造 AllItemLinks 和 AllItemTitles，防止首次更新时变动检测失效
		links := make([]string, len(items))
		titles := make([]string, len(items))
		for i, item := range items {
			links[i] = item.Link
			titles[i] = item.Title
		}

		globals.DbMap[rssURL] = models.Feed{
			Title:         title,
			Link:          rssURL,
			Icon:          icon,
			Items:         items,
			Custom:        map[string]string{"lastupdate": "已加载缓存"},
			AllItemLinks:  links,
			AllItemTitles: titles,
			ShowPubDate:   showPubDate,
			ShowCategory:  showCategory,
		}
	}
	globals.Lock.Unlock()
	
	log.Printf("[数据加载] 条目缓存: 已加载 %d 个源", len(cache))
}

// MarkDataChanged 标记数据已更改
func MarkDataChanged() {
	dataChangedLock.Lock()
	dataChanged = true
	dataChangedLock.Unlock()
}

// autoSaveLoop 自动保存循环
func autoSaveLoop() {
	ticker := time.NewTicker(time.Duration(SaveInterval) * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		dataChangedLock.Lock()
		needSave := dataChanged
		dataChanged = false
		dataChangedLock.Unlock()
		
		if needSave {
			SaveAllData()
		}
	}
}

// SaveAllData 保存所有数据到数据库
func SaveAllData() {
	saveClassifyCache()
	saveReadState()
	savePostProcessCache()
	saveItemsCache()
}

// saveClassifyCache 保存分类缓存到数据库
func saveClassifyCache() {
	globals.ClassifyCacheLock.RLock()
	defer globals.ClassifyCacheLock.RUnlock()
	
	for link, entry := range globals.ClassifyCache {
		if err := DBSaveClassifyCache(link, entry.Category); err != nil {
			log.Printf("保存分类缓存失败 [%s]: %v", link, err)
		}
	}
}

// saveReadState 保存已读状态到数据库
func saveReadState() {
	globals.ReadStateLock.RLock()
	states := make(map[string]int64, len(globals.ReadState))
	for k, v := range globals.ReadState {
		states[k] = v
	}
	globals.ReadStateLock.RUnlock()
	
	if err := DBSaveReadStateBatch(states); err != nil {
		log.Printf("保存已读状态失败: %v", err)
	}
}

// savePostProcessCache 保存后处理缓存到数据库
func savePostProcessCache() {
	PostProcessCacheLock.RLock()
	defer PostProcessCacheLock.RUnlock()
	
	for link, entry := range PostProcessCache {
		dbEntry := DBPostProcessEntry{
			Link:        link,
			Title:       entry.Title,
			NewLink:     entry.Link,
			PubDate:     entry.PubDate,
			ProcessedAt: entry.ProcessedAt,
		}
		if err := DBSavePostProcessCache(dbEntry); err != nil {
			log.Printf("保存后处理缓存失败 [%s]: %v", link, err)
		}
	}
}

// saveItemsCache 保存条目缓存到数据库
func saveItemsCache() {
	globals.ItemsCacheLock.RLock()
	defer globals.ItemsCacheLock.RUnlock()
	
	for rssURL, items := range globals.ItemsCache {
		entries := make([]DBItemsCacheEntry, len(items))
		for i, item := range items {
			entries[i] = DBItemsCacheEntry{
				RssURL:       rssURL,
				Title:        item.Title,
				Link:         item.Link,
				OriginalLink: item.OriginalLink,
				PubDate:      item.PubDate,
			}
		}
		if err := DBSaveItemsCache(rssURL, entries); err != nil {
			log.Printf("保存条目缓存失败 [%s]: %v", rssURL, err)
		}
	}
}

// GetItemsCache 获取指定源的条目缓存
func GetItemsCache(rssURL string) ([]models.Item, bool) {
	globals.ItemsCacheLock.RLock()
	defer globals.ItemsCacheLock.RUnlock()
	items, ok := globals.ItemsCache[rssURL]
	return items, ok
}

// SetItemsCache 设置指定源的条目缓存
func SetItemsCache(rssURL string, items []models.Item) {
	globals.ItemsCacheLock.Lock()
	globals.ItemsCache[rssURL] = items
	globals.ItemsCacheLock.Unlock()
	
	// 异步保存到数据库
	go func() {
		entries := make([]DBItemsCacheEntry, len(items))
		for i, item := range items {
			entries[i] = DBItemsCacheEntry{
				RssURL:       rssURL,
				Title:        item.Title,
				Link:         item.Link,
				OriginalLink: item.OriginalLink,
				PubDate:      item.PubDate,
			}
		}
		if err := DBSaveItemsCache(rssURL, entries); err != nil {
			log.Printf("保存条目缓存失败 [%s]: %v", rssURL, err)
		}
	}()
}

// DeleteItemsCache 删除指定源的条目缓存
func DeleteItemsCache(rssURL string) {
	globals.ItemsCacheLock.Lock()
	delete(globals.ItemsCache, rssURL)
	globals.ItemsCacheLock.Unlock()
	
	// 异步从数据库删除
	go func() {
		if err := DBDeleteItemsCacheForURL(rssURL); err != nil {
			log.Printf("删除条目缓存失败 [%s]: %v", rssURL, err)
		}
	}()
}

// GetPostProcessCache 获取后处理缓存条目
func GetPostProcessCache(link string) (*models.PostProcessCacheEntry, bool) {
	PostProcessCacheLock.RLock()
	defer PostProcessCacheLock.RUnlock()
	entry, ok := PostProcessCache[link]
	if !ok {
		return nil, false
	}
	return &entry, true
}

// SetPostProcessCache 设置后处理缓存条目
func SetPostProcessCache(link string, entry models.PostProcessCacheEntry) {
	PostProcessCacheLock.Lock()
	PostProcessCache[link] = entry
	PostProcessCacheLock.Unlock()
	
	// 异步保存到数据库
	go func() {
		dbEntry := DBPostProcessEntry{
			Link:        link,
			Title:       entry.Title,
			NewLink:     entry.Link,
			PubDate:     entry.PubDate,
			ProcessedAt: entry.ProcessedAt,
		}
		if err := DBSavePostProcessCache(dbEntry); err != nil {
			log.Printf("保存后处理缓存失败 [%s]: %v", link, err)
		}
	}()
}

// DeletePostProcessCache 删除后处理缓存条目
func DeletePostProcessCache(link string) {
	PostProcessCacheLock.Lock()
	delete(PostProcessCache, link)
	PostProcessCacheLock.Unlock()
	
	// 异步从数据库删除
	go func() {
		if err := DBDeletePostProcessCache(link); err != nil {
			log.Printf("删除后处理缓存失败 [%s]: %v", link, err)
		}
	}()
}

// GetReadState 获取所有已读状态
func GetReadState() map[string]int64 {
	globals.ReadStateLock.RLock()
	defer globals.ReadStateLock.RUnlock()
	
	// 返回副本避免并发问题
	result := make(map[string]int64, len(globals.ReadState))
	for k, v := range globals.ReadState {
		result[k] = v
	}
	return result
}

// IsRead 检查文章是否已读
func IsRead(link string) bool {
	globals.ReadStateLock.RLock()
	defer globals.ReadStateLock.RUnlock()
	_, ok := globals.ReadState[link]
	return ok
}

// MarkRead 标记文章为已读
func MarkRead(link string) {
	now := time.Now().Unix()
	globals.ReadStateLock.Lock()
	globals.ReadState[link] = now
	globals.ReadStateLock.Unlock()
	
	// 异步保存到数据库
	go func() {
		if err := DBSaveReadState(link, now); err != nil {
			log.Printf("保存已读状态失败 [%s]: %v", link, err)
		}
	}()
}

// MarkReadBatch 批量标记文章为已读
func MarkReadBatch(links []string) {
	now := time.Now().Unix()
	states := make(map[string]int64, len(links))
	
	globals.ReadStateLock.Lock()
	for _, link := range links {
		globals.ReadState[link] = now
		states[link] = now
	}
	globals.ReadStateLock.Unlock()
	
	// 异步保存到数据库
	go func() {
		if err := DBSaveReadStateBatch(states); err != nil {
			log.Printf("批量保存已读状态失败: %v", err)
		}
	}()
}

// MarkUnread 标记文章为未读
func MarkUnread(link string) {
	globals.ReadStateLock.Lock()
	delete(globals.ReadState, link)
	globals.ReadStateLock.Unlock()
	
	// 异步从数据库删除
	go func() {
		if err := DBDeleteReadState(link); err != nil {
			log.Printf("删除已读状态失败 [%s]: %v", link, err)
		}
	}()
}

// ClearAllReadState 清除所有已读状态
func ClearAllReadState() {
	globals.ReadStateLock.Lock()
	globals.ReadState = make(map[string]int64)
	globals.ReadStateLock.Unlock()
	
	// 异步从数据库清空
	go func() {
		if err := DBClearReadState(); err != nil {
			log.Printf("清空已读状态失败: %v", err)
		}
	}()
}

// Shutdown 关闭时保存数据
func Shutdown() {
	log.Println("正在保存持久化数据...")
	SaveAllData()
	CloseDatabase()
	log.Println("持久化数据保存完成")
}

// autoCleanupLoop 自动清理循环
func autoCleanupLoop() {
	ticker := time.NewTicker(time.Duration(CleanupInterval) * time.Hour)
	defer ticker.Stop()
	
	for range ticker.C {
		if isDbMapReady() {
			cleanupPersistentData()
		} else {
			log.Println("跳过定期清理：DbMap 为空，可能存在网络问题")
		}
	}
}

// isDbMapReady 检查 DbMap 是否已准备好
func isDbMapReady() bool {
	globals.Lock.RLock()
	defer globals.Lock.RUnlock()
	
	allUrls := globals.RssUrls.GetAllUrls()
	if len(allUrls) == 0 {
		return true
	}
	
	loadedCount := 0
	for _, url := range allUrls {
		if _, ok := globals.DbMap[url]; ok {
			loadedCount++
		}
	}
	
	return loadedCount >= len(allUrls) || (len(allUrls) > 0 && loadedCount >= (len(allUrls)*4/5))
}

// cleanupPersistentData 清理持久化数据
func cleanupPersistentData() {
	log.Println("开始清理持久化数据...")
	
	validLinks := collectValidArticleLinks()
	
	if len(validLinks) == 0 {
		log.Println("清理跳过：没有有效的文章链接（DbMap 可能为空）")
		return
	}
	
	cleanedClassifyCache := cleanupClassifyCache(validLinks)
	cleanedReadState := cleanupReadState(validLinks)
	
	validLinksWithPostProcess := collectValidLinksWithPostProcess()
	cleanedPostProcessCache := cleanupPostProcessCache(validLinksWithPostProcess)
	
	cleanedItemsCache := cleanupItemsCache()
	
	// 清理过期的图标缓存 (1天)
	cleanedIcons, err := DBCleanupIconCache(1)
	if err != nil {
		log.Printf("[数据清理] 图标缓存清理失败: %v", err)
	}

	if cleanedClassifyCache > 0 || cleanedReadState > 0 || cleanedPostProcessCache > 0 || cleanedItemsCache > 0 || cleanedIcons > 0 {
		log.Printf("[数据清理] 清理完成: 分类缓存 %d 条，已读状态 %d 条，后处理缓存 %d 条，条目缓存 %d 个源，图标缓存 %d 条", 
			cleanedClassifyCache, cleanedReadState, cleanedPostProcessCache, cleanedItemsCache, cleanedIcons)
	} else {
		log.Println("[数据清理] 清理完成: 暂无需要清理的数据")
	}
}

// collectValidArticleLinks 收集所有当前有效的文章链接
func collectValidArticleLinks() map[string]bool {
	validLinks := make(map[string]bool)
	
	globals.Lock.RLock()
	for _, feed := range globals.DbMap {
		for _, link := range feed.AllItemLinks {
			validLinks[link] = true
		}
		for _, item := range feed.Items {
			validLinks[item.Link] = true
			if item.OriginalLink != "" {
				validLinks[item.OriginalLink] = true
			}
		}
	}
	globals.Lock.RUnlock()
	
	globals.ItemsCacheLock.RLock()
	for _, items := range globals.ItemsCache {
		for _, item := range items {
			validLinks[item.Link] = true
			if item.OriginalLink != "" {
				validLinks[item.OriginalLink] = true
			}
		}
	}
	globals.ItemsCacheLock.RUnlock()
	
	return validLinks
}

// collectValidLinksWithPostProcess 收集启用了后处理的RSS源的文章链接
func collectValidLinksWithPostProcess() map[string]bool {
	validLinks := make(map[string]bool)
	
	postProcessEnabledUrls := make(map[string]bool)
	for _, source := range globals.RssUrls.Sources {
		if source.URL != "" && source.PostProcess != nil && source.PostProcess.Enabled {
			postProcessEnabledUrls[source.URL] = true
		}
	}
	
	globals.Lock.RLock()
	for rssURL, feed := range globals.DbMap {
		if !postProcessEnabledUrls[rssURL] {
			continue
		}
		
		if len(feed.AllItemLinks) > 0 {
			for _, link := range feed.AllItemLinks {
				validLinks[link] = true
			}
		}
		for _, item := range feed.Items {
			validLinks[item.Link] = true
			if item.OriginalLink != "" {
				validLinks[item.OriginalLink] = true
			}
		}
	}
	globals.Lock.RUnlock()
	
	globals.ItemsCacheLock.RLock()
	for url, items := range globals.ItemsCache {
		if postProcessEnabledUrls[url] {
			for _, item := range items {
				validLinks[item.Link] = true
				if item.OriginalLink != "" {
					validLinks[item.OriginalLink] = true
				}
			}
		}
	}
	globals.ItemsCacheLock.RUnlock()
	
	return validLinks
}

// cleanupClassifyCache 清理分类缓存中不再有效的条目
func cleanupClassifyCache(validLinks map[string]bool) int {
	globals.ClassifyCacheLock.Lock()
	defer globals.ClassifyCacheLock.Unlock()
	
	var toDelete []string
	for link := range globals.ClassifyCache {
		if !validLinks[link] {
			toDelete = append(toDelete, link)
		}
	}
	
	for _, link := range toDelete {
		delete(globals.ClassifyCache, link)
	}
	
	// 从数据库删除
	if len(toDelete) > 0 {
		go DBDeleteClassifyCacheBatch(toDelete)
	}
	
	return len(toDelete)
}

// cleanupReadState 清理已读状态中不再有效的条目
func cleanupReadState(validLinks map[string]bool) int {
	globals.ReadStateLock.Lock()
	defer globals.ReadStateLock.Unlock()
	
	now := time.Now().Unix()
	gracePeriod := int64(1 * 24 * 3600) // 1 天保留期
	
	var toDelete []string
	for link, readAt := range globals.ReadState {
		if validLinks[link] {
			continue
		}
		if now-readAt < gracePeriod {
			continue
		}
		toDelete = append(toDelete, link)
	}
	
	for _, link := range toDelete {
		delete(globals.ReadState, link)
	}
	
	// 从数据库删除
	if len(toDelete) > 0 {
		go DBDeleteReadStateBatch(toDelete)
	}
	
	return len(toDelete)
}

// cleanupPostProcessCache 清理后处理缓存中不再有效的条目
func cleanupPostProcessCache(validLinks map[string]bool) int {
	PostProcessCacheLock.Lock()
	defer PostProcessCacheLock.Unlock()
	
	var toDelete []string
	for link := range PostProcessCache {
		if !validLinks[link] {
			toDelete = append(toDelete, link)
		}
	}
	
	for _, link := range toDelete {
		delete(PostProcessCache, link)
	}
	
	// 从数据库删除
	if len(toDelete) > 0 {
		go DBDeletePostProcessCacheBatch(toDelete)
	}
	
	return len(toDelete)
}

// cleanupItemsCache 清理条目缓存中不再启用缓存的源
// cacheItems: -1表示禁用缓存，0表示自动缓存所有过滤后的条目，>0表示缓存指定数量
func cleanupItemsCache() int {
	// 收集所有启用了缓存的源的URL（cacheItems >= 0 即启用缓存）
	validUrls := make(map[string]bool)
	for _, source := range globals.RssUrls.Sources {
		// cacheItems >= 0 表示启用缓存（0为自动，>0为指定数量），-1表示禁用
		if source.URL != "" && source.CacheItems >= 0 {
			validUrls[source.URL] = true
		}
	}
	
	globals.ItemsCacheLock.Lock()
	defer globals.ItemsCacheLock.Unlock()
	
	var toDelete []string
	for url := range globals.ItemsCache {
		if !validUrls[url] {
			toDelete = append(toDelete, url)
		}
	}
	
	for _, url := range toDelete {
		delete(globals.ItemsCache, url)
	}
	
	// 从数据库删除
	if len(toDelete) > 0 {
		go DBDeleteItemsCacheForURLs(toDelete)
	}
	
	return len(toDelete)
}

// GetCacheItems 获取指定URL的缓存条目数配置
// 返回值: -1表示禁用缓存，0表示自动缓存所有过滤后的条目，>0表示缓存指定数量
// 注意：未在配置中找到的源默认返回0（自动缓存）
func GetCacheItems(rssURL string) int {
	for _, source := range globals.RssUrls.Sources {
		if source.URL == rssURL {
			return source.CacheItems
		}
	}
	return 0
}

// SaveConfig 保存配置到 config.json
func SaveConfig(config models.Config) error {
	data, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return err
	}
	
	f, err := os.OpenFile("config.json", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	
	_, err = f.Write(data)
	return err
}

// CleanupPostProcessCacheOnConfigChange 配置变更时立即清理后处理缓存
func CleanupPostProcessCacheOnConfigChange() {	
	if !isDbMapReady() {
		return
	}
	validLinksWithPostProcess := collectValidLinksWithPostProcess()
	cleaned := cleanupPostProcessCache(validLinksWithPostProcess)
	
	if cleaned > 0 {
		log.Printf("后处理缓存清理: 已清理 %d 条", cleaned)
	}
}

// CleanupItemsCacheOnConfigChange 配置变更时立即清理条目缓存
func CleanupItemsCacheOnConfigChange() {
	cleaned := cleanupItemsCache()
	
	if cleaned > 0 {
		log.Printf("条目缓存清理: 已清理 %d 个源", cleaned)
	}
}

// CleanupReadStateOnConfigChange 配置变更时立即清理已读状态
func CleanupReadStateOnConfigChange() {
	if !isDbMapReady() {
		return
	}
	
	validLinks := collectValidArticleLinks()
	cleaned := cleanupReadState(validLinks)
	
	if cleaned > 0 {
		log.Printf("[已读状态清理] 由于超过 1 天或订阅源变更，%d 条过期记录被清理", cleaned)
	}
}

// ClearClassifyCacheForSource 清除指定源的AI分类缓存
func ClearClassifyCacheForSource(rssURL string) int {
	articleLinks := collectArticleLinksForSource(rssURL)
	
	if len(articleLinks) == 0 {
		return 0
	}
	
	globals.ClassifyCacheLock.Lock()
	defer globals.ClassifyCacheLock.Unlock()
	
	var toDelete []string
	for link := range articleLinks {
		if _, exists := globals.ClassifyCache[link]; exists {
			delete(globals.ClassifyCache, link)
			toDelete = append(toDelete, link)
		}
	}
	
	if len(toDelete) > 0 {
		go DBDeleteClassifyCacheBatch(toDelete)
		log.Printf("[缓存清除] 清除源 %s 的AI分类缓存: %d 条", rssURL, len(toDelete))
	}
	
	return len(toDelete)
}

// ClearPostProcessCacheForSource 清除指定源的后处理缓存
func ClearPostProcessCacheForSource(rssURL string) int {
	articleLinks := collectArticleLinksForSource(rssURL)
	
	if len(articleLinks) == 0 {
		return 0
	}
	
	PostProcessCacheLock.Lock()
	defer PostProcessCacheLock.Unlock()
	
	var toDelete []string
	for link := range articleLinks {
		if _, exists := PostProcessCache[link]; exists {
			delete(PostProcessCache, link)
			toDelete = append(toDelete, link)
		}
	}
	
	if len(toDelete) > 0 {
		go DBDeletePostProcessCacheBatch(toDelete)
		log.Printf("[缓存清除] 清除源 %s 的后处理缓存: %d 条", rssURL, len(toDelete))
	}
	
	return len(toDelete)
}

// collectArticleLinksForSource 收集指定源的所有文章链接
func collectArticleLinksForSource(rssURL string) map[string]bool {
	links := make(map[string]bool)
	
	globals.Lock.RLock()
	if feed, exists := globals.DbMap[rssURL]; exists {
		for _, link := range feed.AllItemLinks {
			links[link] = true
		}
		for _, item := range feed.Items {
			links[item.Link] = true
			if item.OriginalLink != "" {
				links[item.OriginalLink] = true
			}
		}
		log.Printf("[缓存清除] 从 DbMap 找到源 [%s], 收集到 %d 个文章链接", rssURL, len(links))
	} else {
		log.Printf("[缓存清除] DbMap 中未找到源 [%s]", rssURL)
	}
	globals.Lock.RUnlock()
	
	itemsCacheCount := 0
	globals.ItemsCacheLock.RLock()
	if items, exists := globals.ItemsCache[rssURL]; exists {
		for _, item := range items {
			links[item.Link] = true
			if item.OriginalLink != "" {
				links[item.OriginalLink] = true
			}
			itemsCacheCount++
		}
	}
	globals.ItemsCacheLock.RUnlock()
	
	if itemsCacheCount > 0 {
		log.Printf("[缓存清除] 从 ItemsCache 补充 %d 个条目，共 %d 个文章链接", itemsCacheCount, len(links))
	}
	
	return links
}

// writeFileAtomic 原子写入文件（先写临时文件再重命名）- 保留用于配置文件
func writeFileAtomic(filePath string, data []byte) error {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建父目录失败: %w", err)
	}
	
	tmpFile := filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, filePath)
}
