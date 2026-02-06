package utils

import (
	"log"
	"net/url"
	"feedora/globals"
	"feedora/models"
	"sort"
	"strings"
	"time"

	"fmt"
	"io"
	"net/http"
	"github.com/fsnotify/fsnotify"
	"github.com/mmcdole/gofeed"
	"sync"
)

var (
	lastUpdateTimes = make(map[string]time.Time)
	lutLock         sync.Mutex
	// 限制全局并发更新数，防止启动时并发过高 (Default: 5)
	feedUpdateSemaphore = make(chan struct{}, 5)
)

func getEffectiveInterval(rssURL string, sourceRefreshCount int) (int, string) {
	now := time.Now().Format("15:04:05")

	// 检查时间段规则 (Schedules)
	for _, s := range globals.RssUrls.Schedules {
		// 跳过无效的时间规则
		if s.StartTime == "" || s.EndTime == "" || s.StartTime == s.EndTime {
			continue
		}

		match := false
		if s.StartTime < s.EndTime {
			match = now >= s.StartTime && now <= s.EndTime
		} else {
			// 跨天情况 (例如 22:00:00 到 08:00:00)
			match = now >= s.StartTime || now <= s.EndTime
		}

		if match {
			// 使用基频+次数逻辑
			count := s.DefaultCount
			if sourceRefreshCount > 0 {
				count = sourceRefreshCount
			}
			interval := s.BaseRefresh * count
			return interval, fmt.Sprintf("时段规则 (%s-%s, 基频:%d, 次数:%d)", s.StartTime, s.EndTime, s.BaseRefresh, count)
		}
	}

	// 没有匹配任何规则，不刷新
	return 0, "未匹配规则"
}

func UpdateFeeds() {
	for {
		now := time.Now()
		formattedTime := now.Format(time.RFC3339)

		var nextGlobalUpdate time.Time

		// 获取当前所有URL的刷新需求
		for _, source := range globals.RssUrls.Sources {
			if source.URL != "" {
				processFeedUpdate(source.URL, source.RefreshCount, formattedTime, now, &nextGlobalUpdate)
			}
		}

		// 更新全局下次更新时间
		globals.Lock.Lock()
		globals.NextUpdateTime = nextGlobalUpdate
		globals.Lock.Unlock()

		time.Sleep(10 * time.Second) // 缩短检查间隔，提高倒计时准确性
	}
}

func processFeedUpdate(urlBack string, sourceRefreshCount int, formattedTime string, now time.Time, nextGlobalUpdate *time.Time) {
	interval, _ := getEffectiveInterval(urlBack, sourceRefreshCount)

	if interval <= 0 {
		return
	}

	lutLock.Lock()
	lastUpdate, ok := lastUpdateTimes[urlBack]
	lutLock.Unlock()

	intervalDuration := time.Duration(interval) * time.Minute

	if !ok || now.Sub(lastUpdate) >= intervalDuration {
		// 执行更新（带重试机制）
		go func(url, formattedTime string) {
			const maxRetries = 3
			const retryDelay = 1 * time.Second

			var lastErr error
			for attempt := 1; attempt <= maxRetries; attempt++ {
				lastErr = UpdateFeed(url, formattedTime, false)
				if lastErr == nil {
					break
				}

				if attempt < maxRetries {
					log.Printf("[源更新重试] URL [%s]: 第 %d 次尝试失败: %v，%d秒后重试...",
						url, attempt, lastErr, int(retryDelay.Seconds()))
					time.Sleep(retryDelay)
				}
			}

			if lastErr != nil {
				log.Printf("[源更新失败] URL [%s]: 已重试 %d 次，最终失败: %v", url, maxRetries, lastErr)
			}
		}(urlBack, formattedTime)

		lutLock.Lock()
		lastUpdateTimes[urlBack] = now
		lutLock.Unlock()

		nextUpdate := now.Add(intervalDuration)
		if nextGlobalUpdate.IsZero() || nextUpdate.Before(*nextGlobalUpdate) {
			*nextGlobalUpdate = nextUpdate
		}
	} else {
		// 计算该源的下次更新时间，用于确定全局下次更新时间
		nextUpdate := lastUpdate.Add(intervalDuration)
		if nextGlobalUpdate.IsZero() || nextUpdate.Before(*nextGlobalUpdate) {
			*nextGlobalUpdate = nextUpdate
		}
	}
}

// GetFaviconURL 根据 RSS URL 获取对应的 favicon URL
func GetFaviconURL(rssURL string) string {
	parsedURL, err := url.Parse(rssURL)
	if err != nil {
		return ""
	}
	// 使用 Google 的 favicon 服务
	if parsedURL.Host != "" {
		return "https://www.google.com/s2/favicons?domain=" + parsedURL.Host + "&sz=64"
	}
	return ""
}

// ProxyIconURL 将原始图标 URL 包装为代理 URL
func ProxyIconURL(originalURL string) string {
	if originalURL == "" {
		return ""
	}
	if strings.HasPrefix(originalURL, "/api/icon?url=") {
		return originalURL
	}
	return "/api/icon?url=" + url.QueryEscape(originalURL)
}

// ShouldIgnoreOriginalPubDate 检查指定URL是否启用了忽略原始发布时间
func ShouldIgnoreOriginalPubDate(rssURL string) bool {
	for _, source := range globals.RssUrls.Sources {
		if source.URL == rssURL {
			return source.IgnoreOriginalPubDate
		}
	}
	return false
}

// IsRankingMode 检查指定URL是否启用了榜单模式
func IsRankingMode(rssURL string) bool {
	for _, source := range globals.RssUrls.Sources {
		if source.URL == rssURL {
			return source.RankingMode
		}
	}
	return false
}

// GetMaxItems 获取指定URL的最大读取条目数限制，返回0表示不限制
func GetMaxItems(rssURL string) int {
	for _, source := range globals.RssUrls.Sources {
		if source.URL == rssURL {
			return source.MaxItems
		}
	}
	return 0
}

// GetCustomIconURL 从配置中获取自定义图标，如果没有则自动获取 favicon
func GetCustomIconURL(rssURL string, customIcon string) string {
	if customIcon != "" {
		return customIcon
	}
	return GetFaviconURL(rssURL)
}

// FetchAndCacheIcon 获取并缓存图标
func FetchAndCacheIcon(iconURL string) ([]byte, string, error) {
	// 尝试从数据库获取
	data, mimeType, ok, err := DBGetIconCache(iconURL)
	if err == nil && ok {
		return data, mimeType, nil
	}

	// 从网络获取
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Get(iconURL)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch icon failed: %s", resp.Status)
	}

	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	mimeType = resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	// 存入数据库
	_ = DBSaveIconCache(iconURL, data, mimeType)

	return data, mimeType, nil
}

func UpdateFeed(url, formattedTime string, isManual bool) error {
	return UpdateFeedWithOptions(url, formattedTime, isManual, false)
}

// UpdateFeedWithOptions 更新Feed，支持强制重新处理选项
func UpdateFeedWithOptions(url, formattedTime string, isManual bool, forceReprocess bool) error {
	// 获取并发锁，限制同时进行的抓取任务数量
	feedUpdateSemaphore <- struct{}{}
	defer func() { <-feedUpdateSemaphore }()

	prefix := "[订阅更新]"
	if isManual {
		prefix = "[手动刷新]"
	}
	if forceReprocess {
		prefix = "[强制重处理]"
	}

	result, err := globals.Fp.ParseURL(url)
	if err != nil {
		errStr := err.Error()
		if strings.HasSuffix(errStr, "EOF") {
			errStr += " (服务器拒绝访问请求)"
		}
		log.Printf("%s [抓取失败] 地址: %s | 详情: %v", prefix, url, errStr)
		return err
	}

	log.Printf("%s [抓取成功] 源: %s | 条目数: %d", prefix, result.Title, len(result.Items))

	// 如果源名称为空，则使用抓取到的标题
	func(u string, title string) {
		if title == "" {
			return
		}
		globals.Lock.Lock()
		defer globals.Lock.Unlock()
		changed := false
		for i := range globals.RssUrls.Sources {
			if globals.RssUrls.Sources[i].URL == u && globals.RssUrls.Sources[i].Name == "" {
				globals.RssUrls.Sources[i].Name = title
				changed = true
				break
			}
		}
		if changed {
			// 保存配置
			if err := SaveConfig(globals.RssUrls); err != nil {
				log.Printf("[配置] 自动更新源名称失败: %v", err)
			} else {
				log.Printf("[配置] 已自动为源 %s 设置名称: %s", u, title)
			}
		}
	}(url, result.Title)

	// 检查是否忽略原始发布时间
	ignoreOriginalPubDate := ShouldIgnoreOriginalPubDate(url)
	// 检查是否启用榜单模式
	rankingMode := IsRankingMode(url)

	// 快速判断内容是否有更新
	globals.Lock.RLock()
	cache, ok := globals.DbMap[url]
	globals.Lock.RUnlock()

	// 应用最大条目数限制（提前准备用于比对的切片）
	maxItems := GetMaxItems(url)
	checkItems := result.Items
	if maxItems > 0 && len(checkItems) > maxItems {
		checkItems = checkItems[:maxItems]
	}

	shouldUpdateDisplayTime := true
	if ok && len(checkItems) > 0 && !forceReprocess {
		isChanged := false
		hasNewItems := false

		// 检查是否有新文章（链接不在旧列表中）
		oldLinksMap := make(map[string]bool)
		for _, link := range cache.AllItemLinks {
			oldLinksMap[link] = true
		}
		for _, item := range checkItems {
			if !oldLinksMap[item.Link] {
				hasNewItems = true
				isChanged = true
				break
			}
		}

		// 如果还没有发现新文章，检查顺序或标题是否变化
		if !isChanged {
			if len(checkItems) != len(cache.AllItemLinks) || len(checkItems) != len(cache.AllItemTitles) {
				isChanged = true
			} else {
				for i, item := range checkItems {
					if item.Link != cache.AllItemLinks[i] || item.Title != cache.AllItemTitles[i] {
						isChanged = true
						break
					}
				}
			}
		}

		if !isChanged {
			if isManual {
				log.Printf("%s [无新内容] 源: %s | 内容与顺序均未发生变化", prefix, result.Title)
			}

			// 仅在重启后（标记为“已加载缓存”）且抓取成功时，才强制更新时间
			globals.Lock.Lock()
			if c, exists := globals.DbMap[url]; exists && c.Custom != nil && c.Custom["lastupdate"] == "已加载缓存" {
				c.Custom["lastupdate"] = formattedTime
				globals.DbMap[url] = c
			}
			globals.Lock.Unlock()

			return nil
		}

		// 关键逻辑：如果忽略原始发布时间，且没有新文章出现（只是排名变动或标题变动），不更新展示时间
		if ignoreOriginalPubDate && !hasNewItems {
			shouldUpdateDisplayTime = false
		}
	}

	// 获取图标：优先级 1.配置的自定义图标 2.RSS feed的image 3.自动生成favicon
	icon := GetIconForFeed(url, result)

	// 构建缓存条目的时间戳映射（用于恢复没有发布时间的条目）
	cachedPubDates := make(map[string]string)
	cachedFetchTimes := make(map[string]string)
	// 优先从内存缓存获取
	globals.Lock.RLock()
	if cache, ok := globals.DbMap[url]; ok {
		for _, item := range cache.Items {
			if item.PubDate != "" {
				cachedPubDates[item.Link] = item.PubDate
			}
			if item.FetchTime != "" {
				cachedFetchTimes[item.Link] = item.FetchTime
			}
		}
	}
	globals.Lock.RUnlock()
	// 补充从持久化缓存获取
	if cachedItems, ok := GetItemsCache(url); ok {
		for _, item := range cachedItems {
			if item.PubDate != "" {
				if _, exists := cachedPubDates[item.Link]; !exists {
					cachedPubDates[item.Link] = item.PubDate
				}
			}
			if item.FetchTime != "" {
				if _, exists := cachedFetchTimes[item.Link]; !exists {
					cachedFetchTimes[item.Link] = item.FetchTime
				}
			}
		}
	}

	// 先构建所有Items
	allItems := make([]models.Item, 0, len(result.Items))
	rankingBaseTime := time.Now()
	for idx, v := range result.Items {
		pubDate := ""
		fetchTime := ""

		if rankingMode {
			// 榜单模式：每次都按照原始排列顺序分配递减的时间戳，确保排序后保持RSS源的原始顺序
			// 不从缓存读取发布时间
			pubDate = rankingBaseTime.Add(-time.Duration(idx) * time.Second).Format(time.RFC3339)
		} else if ignoreOriginalPubDate {
			// 强制增量模式：总是从缓存恢复或使用当前时间
			if cached, ok := cachedPubDates[v.Link]; ok {
				pubDate = cached
			} else {
				pubDate = formattedTime
			}
		} else {
			// 正常模式：优先使用RSS自带的时间戳
			if v.PublishedParsed != nil {
				pubDate = v.PublishedParsed.Format(time.RFC3339)
			} else if v.UpdatedParsed != nil {
				pubDate = v.UpdatedParsed.Format(time.RFC3339)
			} else {
				// RSS没有时间戳，从缓存恢复或使用当前时间
				if cached, ok := cachedPubDates[v.Link]; ok {
					pubDate = cached
				} else {
					pubDate = formattedTime
				}
			}
		}

		// 抓取时间逻辑：优先从缓存恢复，否则使用当前时间
		if cached, ok := cachedFetchTimes[v.Link]; ok {
			fetchTime = cached
		} else {
			fetchTime = formattedTime
		}

		allItems = append(allItems, models.Item{
			Link:          v.Link,
			Title:         v.Title,
			Description:   v.Description,
			Source:        result.Title,
			PubDate:       pubDate,
			FetchTime:     fetchTime,
			OriginalIndex: idx, // 记录在RSS源中的原始索引
		})
	}

	// 应用最大条目数限制
	// maxItems 已经在上面获取过了
	if maxItems > 0 && len(allItems) > maxItems {
		allItems = allItems[:maxItems]
	}

	// 应用AI分类和过滤
	originalCount := len(allItems)
	filteredItems := allItems
	passedLinks := make(map[string]bool)

	if ShouldFilter(url) {
		log.Printf("%s [开始分类] 源: %s | 待处理条目: %d", prefix, result.Title, originalCount)
		// 使用新的分类函数，它会同时处理分类和过滤
		filteredItems = ClassifyItems(allItems, url)
		for _, item := range filteredItems {
			passedLinks[item.Link] = true
		}
		// 更新allItems中的分类信息
		categoryMap := make(map[string]string)
		for _, item := range filteredItems {
			categoryMap[item.Link] = item.Category
		}
		for i := range allItems {
			if cat, ok := categoryMap[allItems[i].Link]; ok {
				allItems[i].Category = cat
			}
		}
	} else {
		// 如果不启用过滤，所有条目都视为通过
		for _, item := range allItems {
			passedLinks[item.Link] = true
		}
	}

	// 按时间戳降序排序（确保所有条目按时间排列，新条目自然排在最前）
	// 当时间戳相同时，按原始索引升序排列，保持RSS源中的原始顺序
	sort.SliceStable(allItems, func(i, j int) bool {
		if allItems[i].PubDate == allItems[j].PubDate {
			return allItems[i].OriginalIndex < allItems[j].OriginalIndex
		}
		return allItems[i].PubDate > allItems[j].PubDate
	})

	// 重新构建过滤后的列表，以反映排序变化
	if len(passedLinks) < len(allItems) {
		newFilteredItems := make([]models.Item, 0, len(filteredItems))
		for _, item := range allItems {
			if passedLinks[item.Link] {
				newFilteredItems = append(newFilteredItems, item)
			}
		}
		filteredItems = newFilteredItems
	} else {
		filteredItems = allItems
	}

	// 应用后处理
	if ShouldPostProcess(url) {
		beforePostCount := len(filteredItems)
		filteredItems = PostProcessItems(filteredItems, url)
		log.Printf("%s [后处理完成] 源: %s | 处理条目: %d", prefix, result.Title, beforePostCount)
	}

	// 应用条目缓存逻辑：将旧条目与新条目合并
	// cacheItems: -1表示禁用缓存，0表示自动缓存所有过滤后的条目，>0表示缓存指定数量
	cacheItems := GetCacheItems(url)
	if cacheItems == 0 {
		// 0表示自动缓存所有过滤后的条目
		cacheItems = len(filteredItems)
	}
	if cacheItems > 0 {
		beforeMergeCount := len(filteredItems)
		filteredItems = mergeWithCachedItems(url, filteredItems, cacheItems)
		log.Printf("%s [缓存合并] 源: %s | 合并前: %d，合并后: %d", prefix, result.Title, beforeMergeCount, len(filteredItems))
	}

	// 记录过滤前的所有文章链接和标题，用于清理和变动检测
	allItemLinks := make([]string, 0, len(allItems))
	allItemTitles := make([]string, 0, len(allItems))
	for _, item := range allItems {
		allItemLinks = append(allItemLinks, item.Link)
		allItemTitles = append(allItemTitles, item.Title)
	}

	// 即时清理该源已不存在的文章缓存（AI过滤缓存、后处理缓存、榜单时间戳等）
	// 这确保了在两次全量清理之间，单个源的更新也能保持缓存精简
	// 注意：先在主线程中获取旧缓存数据的快照，避免在 goroutine 中读取时 DbMap 已被更新
	var oldLinks []string
	var oldItemLinks []string
	globals.Lock.RLock()
	if cache, ok := globals.DbMap[url]; ok {
		oldLinks = make([]string, len(cache.AllItemLinks))
		copy(oldLinks, cache.AllItemLinks)
		if len(oldLinks) == 0 {
			for _, item := range cache.Items {
				oldLinks = append(oldLinks, item.Link)
			}
		}
		// 获取旧的展示条目链接
		for _, item := range cache.Items {
			oldItemLinks = append(oldItemLinks, item.Link)
		}
	}
	globals.Lock.RUnlock()

	// 同时获取 ItemsCache 中的条目链接
	if cachedItems, ok := GetItemsCache(url); ok {
		for _, item := range cachedItems {
			oldItemLinks = append(oldItemLinks, item.Link)
		}
	}

	go func(u string, newLinks []string, oldLinks []string, oldItemLinks []string, newFilteredItems []models.Item) {
		// 构建当前源的所有有效链接（包括过滤后的和过滤前的备选）
		currentLinks := make(map[string]bool)
		for _, l := range newLinks {
			currentLinks[l] = true
		}
		// 包含新的过滤后条目链接
		for _, item := range newFilteredItems {
			currentLinks[item.Link] = true
		}
		// 包含之前已经在展示中的条目（特别是对于有缓存条目功能的情况）
		for _, l := range oldItemLinks {
			currentLinks[l] = true
		}

		// 清理 AI 过滤缓存（基于旧的 AllItemLinks）
		if len(oldLinks) > 0 {
			cleanupClassifyCacheForSource(oldLinks, currentLinks)
		}

		// 清理后处理缓存
		if len(oldLinks) > 0 && ShouldPostProcess(u) {
			cleanupPostProcessCacheForSource(oldLinks, currentLinks)
		}
	}(url, allItemLinks, oldLinks, oldItemLinks, filteredItems)

	// 确定最终展示的更新时间（优先使用条目中最新的抓取时间）
	lastUpdateTime := ""
	for _, item := range filteredItems {
		if item.FetchTime != "" {
			if lastUpdateTime == "" || item.FetchTime > lastUpdateTime {
				lastUpdateTime = item.FetchTime
			}
		}
	}

	if lastUpdateTime == "" {
		// 如果没有条目，则回退到原有的逻辑
		lastUpdateTime = formattedTime
		if !shouldUpdateDisplayTime && ok && cache.Custom["lastupdate"] != "已加载缓存" {
			lastUpdateTime = cache.Custom["lastupdate"]
		}
	}

	customFeed := models.Feed{
		Title:         result.Title,
		Link:          url,
		Icon:          icon,
		Custom:        map[string]string{"lastupdate": lastUpdateTime},
		Items:         filteredItems,
		FilteredCount: originalCount - len(filteredItems),
		AllItemLinks:  allItemLinks,
		AllItemTitles: allItemTitles,
	}

	globals.Lock.Lock()
	defer globals.Lock.Unlock()
	globals.DbMap[url] = customFeed
	log.Printf("%s [更新完成] 源: %s | 最终条目数: %d", prefix, result.Title, len(filteredItems))
	return nil
}

// cleanupClassifyCacheForSource 清理源中已不存在的条目的分类缓存
func cleanupClassifyCacheForSource(oldLinks []string, newLinks map[string]bool) {
	globals.ClassifyCacheLock.Lock()
	defer globals.ClassifyCacheLock.Unlock()

	for _, link := range oldLinks {
		if !newLinks[link] {
			// 该条目不再存在于新列表中，清理其分类缓存
			delete(globals.ClassifyCache, link)
		}
	}
}

// cleanupPostProcessCacheForSource 清理指定源中已不存在条目的后处理缓存
func cleanupPostProcessCacheForSource(oldLinks []string, newLinks map[string]bool) {
	PostProcessCacheLock.Lock()
	defer PostProcessCacheLock.Unlock()

	for _, link := range oldLinks {
		if !newLinks[link] {
			delete(PostProcessCache, link)
		}
	}
}

// mergeWithCachedItems 将新条目与缓存的旧条目合并，保持总数达到 cacheItems
func mergeWithCachedItems(url string, newItems []models.Item, cacheItems int) []models.Item {
	// 构建链接集合用于去重，并首先对新条目内部去重
	uniqueNewItems := make([]models.Item, 0, len(newItems))
	newLinks := make(map[string]bool)
	for _, item := range newItems {
		if item.Link != "" && !newLinks[item.Link] {
			newLinks[item.Link] = true
			uniqueNewItems = append(uniqueNewItems, item)
		}
	}
	newItems = uniqueNewItems

	// 从缓存中获取旧条目
	cachedItems, hasCached := GetItemsCache(url)

	// 合并条目：新条目 + 不在新条目中的旧条目
	mergedItems := make([]models.Item, 0, cacheItems)
	mergedItems = append(mergedItems, newItems...)

	if hasCached {
		for _, item := range cachedItems {
			// 只添加不在新列表中的旧条目，且旧条目本身也要去重（以防万一）
			if item.Link != "" && !newLinks[item.Link] {
				newLinks[item.Link] = true
				mergedItems = append(mergedItems, item)
			}
			// 达到缓存数量限制后停止
			if len(mergedItems) >= cacheItems {
				break
			}
		}
	}

	// 限制总数不超过 cacheItems
	if len(mergedItems) > cacheItems {
		mergedItems = mergedItems[:cacheItems]
	}

	// 清除不需要的字段后保存到缓存（节省存储空间）
	cachedItemsToSave := make([]models.Item, len(mergedItems))
	for i, item := range mergedItems {
		cachedItemsToSave[i] = models.Item{
			Title:        item.Title,
			Link:         item.Link,
			OriginalLink: item.OriginalLink, // 保留原始链接用于后处理缓存查询
			PubDate:      item.PubDate,
			FetchTime:    item.FetchTime,   // 保留抓取时间
			Category:     item.Category,    // 保留分类信息
			// Description 和 Source 字段不保存到缓存
		}
	}
	SetItemsCache(url, cachedItemsToSave)

	return mergedItems
}

// GetIconForURL 从配置中获取 URL 对应的自定义图标，如果没有则自动生成 favicon
func GetIconForURL(rssURL string) string {
	iconURL := ""
	// 检查 sources 配置中是否有自定义图标
	for _, source := range globals.RssUrls.Sources {
		if source.URL == rssURL && source.Icon != "" {
			iconURL = source.Icon
			break
		}
	}
	if iconURL == "" {
		// 没有自定义图标，使用自动获取的 favicon
		iconURL = GetFaviconURL(rssURL)
	}
	return ProxyIconURL(iconURL)
}

// GetIconForFeed 获取feed的图标，优先级：1.配置的自定义图标 2.RSS的image字段 3.自动生成favicon
func GetIconForFeed(rssURL string, feed interface{}) string {
	iconURL := ""
	// 1. 先检查配置中是否有自定义图标
	for _, source := range globals.RssUrls.Sources {
		if source.URL == rssURL && source.Icon != "" {
			iconURL = source.Icon
			break
		}
	}

	if iconURL == "" {
		// 2. 尝试从RSS feed的image字段获取
		if feedResult, ok := feed.(*gofeed.Feed); ok {
			if feedResult.Image != nil && feedResult.Image.URL != "" {
				iconURL = feedResult.Image.URL
			}
		}
	}

	if iconURL == "" {
		// 3. 最后使用自动获取的 favicon
		iconURL = GetFaviconURL(rssURL)
	}

	return ProxyIconURL(iconURL)
}

// GetFeeds 获取feeds列表，根据布局分组返回
func GetFeeds() []models.Feed {
	feeds := make([]models.Feed, 0)

	// 遍历所有分组布局
	for _, layoutGroup := range globals.RssUrls.LayoutGroups {
		// 遍历该分组中的所有布局项
		for _, item := range layoutGroup.Items {
			if item.Type == "source" && item.SourceURL != "" {
				// 单个源
				feed := buildSourceFeed(item.SourceURL, layoutGroup.Name)
				if feed != nil {
					feeds = append(feeds, *feed)
				}
			} else if item.Type == "folder" && item.FolderID != "" {
				// 文件夹
				folder := globals.RssUrls.GetFolderByID(item.FolderID)
				if folder != nil {
					feed := buildFolderFeed(*folder, layoutGroup.Name)
					if feed != nil {
						feeds = append(feeds, *feed)
					}
				}
			}
		}
	}

	return feeds
}

// buildSourceFeed 构建单个源的Feed
func buildSourceFeed(sourceURL string, groupName string) *models.Feed {
	source := globals.RssUrls.GetSourceByURL(sourceURL)
	if source == nil {
		return nil
	}

	globals.Lock.RLock()
	cache, ok := globals.DbMap[source.URL]
	globals.Lock.RUnlock()

	if !ok {
		// 返回空的Feed对象，展示卡片但内容为空
		title := "加载中"
		if source.Name != "" {
			title = source.Name
		}
		return &models.Feed{
			Title:  title,
			Link:   source.URL,
			Icon:   source.Icon,
			Custom: map[string]string{"lastupdate": "加载中"},
			Items:  []models.Item{},
			Group:  groupName,
		}
	}

	// 复制缓存以避免修改原始数据
	result := cache
	
	// 支持自定义名称
	if source.Name != "" {
		result.Title = source.Name
	}
	// 支持自定义图标
	if source.Icon != "" {
		result.Icon = ProxyIconURL(source.Icon)
	}
	result.Group = groupName
	// 设置是否显示发布时间
	result.ShowPubDate = source.ShowPubDate
	// 设置是否显示分类标签
	result.ShowCategory = source.ShowCategory
	// 设置是否为榜单模式
	result.RankingMode = source.RankingMode

	return &result
}

// buildFolderFeed 构建文件夹Feed，聚合多个源的内容
func buildFolderFeed(folder models.Folder, groupName string) *models.Feed {
	icon := folder.Icon
	if icon != "" {
		icon = ProxyIconURL(icon)
	} else if len(folder.Entries) > 0 {
		firstEntry := folder.Entries[0]
		if firstEntry.SourceURL != "" {
			// 如果第一个是普通订阅源，使用该源的图标
			source := globals.RssUrls.GetSourceByURL(firstEntry.SourceURL)
			if source != nil {
				globals.Lock.RLock()
				if cache, ok := globals.DbMap[source.URL]; ok {
					icon = cache.Icon
				}
				globals.Lock.RUnlock()

				// 如果缓存中还没有，尝试获取默认图标
				if icon == "" {
					icon = GetIconForFeed(source.URL, nil)
				}
			}
		}
		// 如果第一个是分类包 (CategoryPackageId != "")，使用默认逻辑 (即保持为空，由前端或后续逻辑处理)
	}

	folderFeed := &models.Feed{
		Title:       folder.Name,
		Link:        "folder:" + folder.ID,
		Icon:        icon,
		IsFolder:    true,
		Custom:      map[string]string{"lastupdate": "加载中"},
		Items:       make([]models.Item, 0),
		ShowPubDate:  folder.ShowPubDate,
		ShowCategory: folder.ShowCategory,
		ShowSource:   folder.ShowSource,
		Group:        groupName,
	}

	// 遍历文件夹条目
	for _, entry := range folder.Entries {
		// 确定要过滤的类别列表
		var categories []string
		if len(entry.Categories) > 0 {
			categories = entry.Categories
		}

		// 确定是否隐藏源名称
		hideSource := entry.HideSource

		if entry.CategoryPackageId != "" {
			// 分类包条目 - 添加该分类包对应的所有订阅源
			packageSources := globals.RssUrls.GetSourcesByPackageId(entry.CategoryPackageId)
			for _, pkgSource := range packageSources {
				addSourceItemsToFolder(folderFeed, pkgSource.URL, pkgSource.Name, categories, hideSource)
			}
		} else if entry.SourceURL != "" {
			// 普通订阅源条目
			source := globals.RssUrls.GetSourceByURL(entry.SourceURL)
			sourceName := ""
			if source != nil {
				sourceName = source.Name
			}
			addSourceItemsToFolder(folderFeed, entry.SourceURL, sourceName, categories, hideSource)
		}
	}

	// 按发布时间倒序排列
	sort.SliceStable(folderFeed.Items, func(i, j int) bool {
		pubDateI := folderFeed.Items[i].PubDate
		pubDateJ := folderFeed.Items[j].PubDate

		if pubDateI == "" && pubDateJ == "" {
			return false
		}
		if pubDateI == "" {
			return false
		}
		if pubDateJ == "" {
			return true
		}
		return pubDateI > pubDateJ
	})

	// 根据标题去重
	seenTitles := make(map[string]bool)
	uniqueItems := make([]models.Item, 0, len(folderFeed.Items))
	for _, item := range folderFeed.Items {
		normalizedTitle := strings.TrimSpace(item.Title)
		if normalizedTitle == "" {
			continue
		}

		if !seenTitles[normalizedTitle] {
			seenTitles[normalizedTitle] = true
			uniqueItems = append(uniqueItems, item)
		}
	}
	folderFeed.Items = uniqueItems

	return folderFeed
}

// addSourceItemsToFolder 将源的条目添加到文件夹中
func addSourceItemsToFolder(folderFeed *models.Feed, sourceURL string, sourceName string, categoryFilters []string, hideSource bool) {
	globals.Lock.RLock()
	cache, ok := globals.DbMap[sourceURL]
	globals.Lock.RUnlock()

	if !ok {
		// 源未就绪，添加提示项
		name := sourceName
		if name == "" {
			name = "未知源"
		}
		folderFeed.Items = append(folderFeed.Items, models.Item{
			Title:       "⚠️ " + name + " 加载失败",
			Link:        sourceURL,
			Description: "该订阅源暂时无法加载，请稍后重试",
			Source:      name,
			PubDate:     "",
		})
		return
	}

	// 获取源的 lastupdate 作为 FetchTime 的备用值
	sourceLastUpdate := ""
	if cache.Custom != nil {
		sourceLastUpdate = cache.Custom["lastupdate"]
		// 过滤掉非时间字符串
		if sourceLastUpdate == "加载中" || sourceLastUpdate == "已加载缓存" {
			sourceLastUpdate = ""
		}
	}

	// 添加条目
	for _, item := range cache.Items {
		// 如果指定了类别过滤，只添加匹配的条目
		// 类别留空表示忽略类别过滤（直接展示分类后的条目）
		if len(categoryFilters) > 0 {
			match := false
			for _, filter := range categoryFilters {
				if item.Category == filter {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}

		// 更新文件夹的最后更新时间（使用条目的抓取时间，若无则用源的 lastupdate）
		effectiveTime := item.FetchTime
		if effectiveTime == "" {
			effectiveTime = sourceLastUpdate
		}
		if effectiveTime != "" {
			currentTime := folderFeed.Custom["lastupdate"]
			if currentTime == "加载中" || effectiveTime > currentTime {
				folderFeed.Custom["lastupdate"] = effectiveTime
			}
		}

		newItem := item
		if !hideSource {
			newItem.Source = sourceName
		} else {
			newItem.Source = ""
		}
		
		folderFeed.Items = append(folderFeed.Items, newItem)
	}
}

func WatchConfigFileChanges(filePath string) {
	// 创建一个新的监控器
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	// 添加要监控的文件
	err = watcher.Add(filePath)
	if err != nil {
		log.Printf("添加监控失败: %v", err)
	}

	// 启动一个 goroutine 来处理文件变化事件
	go func() {
		var debounceTimer *time.Timer
		const debounceInterval = 500 * time.Millisecond

		reloadFunc := func() {
			log.Println("文件已修改，重新加载配置")

			// 等待文件完全写入，然后重试读取配置
			var oldConfig models.Config
			var err error
			for i := 0; i < 3; i++ {
				if i > 0 {
					time.Sleep(100 * time.Millisecond)
				}
				oldConfig, err = globals.ReloadConfig()
				if err == nil {
					break
				}
				log.Printf("重载配置失败（尝试 %d/3）: %v", i+1, err)
			}

			if err != nil {
				log.Printf("配置重载最终失败，保持使用旧配置: %v", err)
				return
			}

			log.Println("配置重载成功")

			// 1. 立即清理后处理缓存
			CleanupPostProcessCacheOnConfigChange()

			// 2. 立即清理条目缓存（清理不再启用缓存的源）
			CleanupItemsCacheOnConfigChange()

			// 3. 立即清理已读状态（清理已删除源的数据）
			CleanupReadStateOnConfigChange()

			// 收集受影响的源（配置发生变化的源）
			affectedUrls := collectAffectedUrls(oldConfig, globals.RssUrls)

			if len(affectedUrls) == 0 {
				log.Println("配置更新：无源受影响，跳过更新")
				return
			}

			log.Printf("配置更新：%d 个源受影响，开始更新", len(affectedUrls))
			formattedTime := time.Now().Format(time.RFC3339)

			for url := range affectedUrls {
				go UpdateFeedWithOptions(url, formattedTime, true, true)
			}
		}

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				// 忽略无用事件
				if event.Op&fsnotify.Chmod == fsnotify.Chmod {
					continue
				}

				if event.Op&fsnotify.Write == fsnotify.Write ||
					event.Op&fsnotify.Create == fsnotify.Create ||
					event.Op&fsnotify.Rename == fsnotify.Rename {

					// 如果是重命名或创建，尝试重新添加监控（针对某些原子写操作）
					if event.Op&fsnotify.Rename == fsnotify.Rename || event.Op&fsnotify.Remove == fsnotify.Remove {
						// 稍微延迟以确保新文件存在
						go func() {
							time.Sleep(100 * time.Millisecond)
							watcher.Add(filePath)
						}()
					}

					// 防抖动
					if debounceTimer != nil {
						debounceTimer.Stop()
					}
					debounceTimer = time.AfterFunc(debounceInterval, reloadFunc)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("错误:", err)
			}
		}
	}()

	select {}
}

// RefreshSingleFeed 刷新单个源
func RefreshSingleFeed(link string) error {
	formattedTime := time.Now().Format(time.RFC3339)
	log.Printf("[手动刷新] 开始刷新: %s", link)

	// 检查是否是文件夹链接
	if strings.HasPrefix(link, "folder:") {
		folderID := strings.TrimPrefix(link, "folder:")
		folder := globals.RssUrls.GetFolderByID(folderID)
		if folder == nil {
			log.Printf("未找到文件夹: %s", folderID)
			return fmt.Errorf("folder not found")
		}

		log.Printf("[手动刷新] 刷新文件夹 [%s] 中的所有源", folder.Name)
		
		// 收集需要刷新的源URL
		urlsToRefresh := make([]string, 0)
		for _, entry := range folder.Entries {
			if entry.CategoryPackageId != "" {
				// 分类包条目 - 添加该分类包对应的所有订阅源
				for _, src := range globals.RssUrls.GetSourcesByPackageId(entry.CategoryPackageId) {
					urlsToRefresh = append(urlsToRefresh, src.URL)
				}
			} else if entry.SourceURL != "" {
				urlsToRefresh = append(urlsToRefresh, entry.SourceURL)
			}
		}

		// 并发刷新所有源
		var wg sync.WaitGroup
		errChan := make(chan error, len(urlsToRefresh))
		startTime := time.Now()

		for _, url := range urlsToRefresh {
			wg.Add(1)
			go func(u string) {
				defer wg.Done()
				err := UpdateFeed(u, formattedTime, true)
				if err != nil {
					errChan <- err
				}
			}(url)
		}
		wg.Wait()
		close(errChan)

		errorCount := len(errChan)
		duration := time.Since(startTime)
		if errorCount > 0 {
			log.Printf("[手动刷新] 文件夹 [%s] 刷新完成，耗时 %v，共有 %d/%d 个源失败", folder.Name, duration, errorCount, len(urlsToRefresh))
		} else {
			log.Printf("[手动刷新] 文件夹 [%s] 刷新成功，耗时 %v，共 %d 个源", folder.Name, duration, len(urlsToRefresh))
		}
		return nil
	}

	// 单个源刷新
	for _, source := range globals.RssUrls.Sources {
		if source.URL == link {
			startTime := time.Now()
			log.Printf("[手动刷新] 确认匹配单个源: %s", link)

			err := UpdateFeed(source.URL, formattedTime, true)

			duration := time.Since(startTime)
			if err != nil {
				log.Printf("[手动刷新失败] 单个源 [%s] 刷新失败，耗时 %v: %v", link, duration, err)
			} else {
				log.Printf("[手动刷新] 单个源 [%s] 刷新完成，耗时 %v", link, duration)
			}
			return err
		}
	}

	log.Printf("未找到匹配的源: %s", link)
	return fmt.Errorf("feed not found")
}

// RefreshSingleFeedForce 强制刷新单个源并重新处理（跳过内容变化检测）
func RefreshSingleFeedForce(link string) error {
	formattedTime := time.Now().Format(time.RFC3339)
	log.Printf("[强制重处理] 开始刷新: %s", link)

	// 查找匹配的源
	for _, source := range globals.RssUrls.Sources {
		if source.URL == link {
			startTime := time.Now()
			err := UpdateFeedWithOptions(link, formattedTime, true, true)
			duration := time.Since(startTime)
			if err != nil {
				log.Printf("[强制重处理] 源 [%s] 刷新失败，耗时 %v: %v", link, duration, err)
			} else {
				log.Printf("[强制重处理] 源 [%s] 刷新完成，耗时 %v", link, duration)
			}
			return err
		}
	}

	log.Printf("未找到匹配的源: %s", link)
	return fmt.Errorf("feed not found")
}

// ClearFeedCacheForPostProcessSources 清除启用了后处理的源的Feed缓存
// 这样在配置变更后，即使文章内容未变，也会重新获取和处理
func ClearFeedCacheForPostProcessSources() {
	globals.Lock.Lock()
	defer globals.Lock.Unlock()

	cleared := 0
	for rssURL := range globals.DbMap {
		// 检查该源是否启用了后处理
		if ShouldPostProcess(rssURL) {
			delete(globals.DbMap, rssURL)
			cleared++
		}
	}

	if cleared > 0 {
		log.Printf("已清除 %d 个启用后处理的源的Feed缓存", cleared)
	}
}

// collectAffectedUrls 比较新旧配置，收集受影响的源URL
func collectAffectedUrls(oldConfig, newConfig models.Config) map[string]bool {
	affectedUrls := make(map[string]bool)

	// 创建旧配置的源映射
	oldSources := make(map[string]*models.Source)
	for i := range oldConfig.Sources {
		source := &oldConfig.Sources[i]
		if source.URL != "" {
			oldSources[source.URL] = source
		}
	}

	// 检查新配置中的每个源
	for i := range newConfig.Sources {
		source := &newConfig.Sources[i]
		if source.URL != "" {
			// 检查是否是新增的源或配置发生了变化
			if oldSource, exists := oldSources[source.URL]; !exists || sourceChanged(oldSource, source) {
				affectedUrls[source.URL] = true
			}
		}
	}

	return affectedUrls
}

// sourceChanged 检查源配置是否发生了变化
func sourceChanged(old, new *models.Source) bool {
	// 检查影响数据获取或处理的关键配置
	if old.MaxItems != new.MaxItems ||
		old.CacheItems != new.CacheItems ||
		old.IgnoreOriginalPubDate != new.IgnoreOriginalPubDate ||
		old.RankingMode != new.RankingMode {
		return true
	}

	// 检查分类配置是否变化
	if classifyChanged(old.Classify, new.Classify) {
		return true
	}

	// 检查后处理配置是否变化
	if postProcessChanged(old.PostProcess, new.PostProcess) {
		return true
	}

	return false
}

// classifyChanged 检查分类配置是否变化
func classifyChanged(old, new *models.ClassifyStrategy) bool {
	if (old == nil) != (new == nil) {
		return true
	}
	if old == nil {
		return false
	}

	// 比较 KeywordEnabled 字段
	if (old.KeywordEnabled == nil) != (new.KeywordEnabled == nil) {
		return true
	}
	if old.KeywordEnabled != nil && new.KeywordEnabled != nil && *old.KeywordEnabled != *new.KeywordEnabled {
		return true
	}

	// 比较 AIEnabled 字段
	if (old.AIEnabled == nil) != (new.AIEnabled == nil) {
		return true
	}
	if old.AIEnabled != nil && new.AIEnabled != nil && *old.AIEnabled != *new.AIEnabled {
		return true
	}

	// 比较 WhitelistMode 字段
	if (old.WhitelistMode == nil) != (new.WhitelistMode == nil) {
		return true
	}
	if old.WhitelistMode != nil && new.WhitelistMode != nil && *old.WhitelistMode != *new.WhitelistMode {
		return true
	}

	// 比较 ScriptFilterEnabled 字段
	if (old.ScriptFilterEnabled == nil) != (new.ScriptFilterEnabled == nil) {
		return true
	}
	if old.ScriptFilterEnabled != nil && new.ScriptFilterEnabled != nil && *old.ScriptFilterEnabled != *new.ScriptFilterEnabled {
		return true
	}

	// 比较 ScriptFilterContent 字段
	if old.ScriptFilterContent != new.ScriptFilterContent {
		return true
	}

	// 检查关键词列表
	if len(old.FilterKeywords) != len(new.FilterKeywords) || len(old.KeepKeywords) != len(new.KeepKeywords) {
		return true
	}

	return false
}

// postProcessChanged 检查后处理配置是否变化
func postProcessChanged(old, new *models.PostProcessConfig) bool {
	if (old == nil) != (new == nil) {
		return true
	}
	if old == nil {
		return false
	}

	// 比较关键字段
	if old.Enabled != new.Enabled ||
		old.Mode != new.Mode ||
		old.Prompt != new.Prompt ||
		old.ScriptPath != new.ScriptPath ||
		old.ScriptContent != new.ScriptContent ||
		old.ModifyTitle != new.ModifyTitle ||
		old.ModifyLink != new.ModifyLink ||
		old.ModifyPubDate != new.ModifyPubDate {
		return true
	}

	return false
}
