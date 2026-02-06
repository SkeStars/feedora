package utils

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	// DB 数据库连接
	DB *sql.DB
	// DatabaseFile 数据库文件路径
	DatabaseFile = getDatabaseFile()
)

// getDatabaseFile 获取数据库文件路径
func getDatabaseFile() string {
	return filepath.Join(DataDir, "feedora.db")
}

// InitDatabase 初始化数据库
func InitDatabase() error {
	// 确保数据目录存在
	if err := os.MkdirAll(DataDir, 0755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}

	var err error
	DB, err = sql.Open("sqlite3", DatabaseFile+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}

	// 测试连接
	if err = DB.Ping(); err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}

	// 创建表结构
	if err = createTables(); err != nil {
		return fmt.Errorf("创建表结构失败: %w", err)
	}

	log.Printf("[数据库] 初始化完成: %s", DatabaseFile)
	return nil
}

// createTables 创建表结构
func createTables() error {
	// AI分类缓存表
	_, err := DB.Exec(`
		CREATE TABLE IF NOT EXISTS classify_cache (
			link TEXT PRIMARY KEY,
			category TEXT NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("创建 classify_cache 表失败: %w", err)
	}

	// 已读状态表
	_, err = DB.Exec(`
		CREATE TABLE IF NOT EXISTS read_state (
			link TEXT PRIMARY KEY,
			read_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("创建 read_state 表失败: %w", err)
	}

	// 后处理缓存表
	_, err = DB.Exec(`
		CREATE TABLE IF NOT EXISTS postprocess_cache (
			link TEXT PRIMARY KEY,
			title TEXT,
			new_link TEXT,
			pub_date TEXT,
			processed_at TEXT NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("创建 postprocess_cache 表失败: %w", err)
	}

	// 条目缓存表
	_, err = DB.Exec(`
		CREATE TABLE IF NOT EXISTS items_cache (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			rss_url TEXT NOT NULL,
			title TEXT NOT NULL,
			link TEXT NOT NULL,
			original_link TEXT,
			pub_date TEXT,
			UNIQUE(rss_url, link)
		)
	`)
	if err != nil {
		return fmt.Errorf("创建 items_cache 表失败: %w", err)
	}

	// 图标缓存表
	_, err = DB.Exec(`
		CREATE TABLE IF NOT EXISTS icon_cache (
			url TEXT PRIMARY KEY,
			data BLOB NOT NULL,
			mime_type TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("创建 icon_cache 表失败: %w", err)
	}

	// 创建索引
	_, err = DB.Exec(`CREATE INDEX IF NOT EXISTS idx_items_cache_rss_url ON items_cache(rss_url)`)
	if err != nil {
		return fmt.Errorf("创建 items_cache 索引失败: %w", err)
	}

	// 数据库迁移：为 items_cache 添加 fetch_time 列（兼容旧版本）
	_, _ = DB.Exec(`ALTER TABLE items_cache ADD COLUMN fetch_time TEXT`)

	return nil
}

// CloseDatabase 关闭数据库连接
func CloseDatabase() {
	if DB != nil {
		if err := DB.Close(); err != nil {
			log.Printf("[数据库] 关闭失败: %v", err)
		} else {
			log.Println("[数据库] 已关闭")
		}
	}
}

// ===== 分类缓存操作 =====

// DBLoadClassifyCache 从数据库加载分类缓存到内存
func DBLoadClassifyCache() (map[string]string, error) {
	rows, err := DB.Query("SELECT link, category FROM classify_cache")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cache := make(map[string]string)
	for rows.Next() {
		var link, category string
		if err := rows.Scan(&link, &category); err != nil {
			return nil, err
		}
		cache[link] = category
	}
	return cache, rows.Err()
}

// DBSaveClassifyCache 保存分类缓存到数据库
func DBSaveClassifyCache(link, category string) error {
	_, err := DB.Exec(
		"INSERT OR REPLACE INTO classify_cache (link, category) VALUES (?, ?)",
		link, category,
	)
	return err
}

// DBDeleteClassifyCache 删除分类缓存
func DBDeleteClassifyCache(link string) error {
	_, err := DB.Exec("DELETE FROM classify_cache WHERE link = ?", link)
	return err
}

// DBDeleteClassifyCacheBatch 批量删除分类缓存
func DBDeleteClassifyCacheBatch(links []string) error {
	if len(links) == 0 {
		return nil
	}
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("DELETE FROM classify_cache WHERE link = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, link := range links {
		if _, err := stmt.Exec(link); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DBClearClassifyCache 清空分类缓存
func DBClearClassifyCache() error {
	_, err := DB.Exec("DELETE FROM classify_cache")
	return err
}

// ===== 图标缓存操作 =====

// DBSaveIconCache 保存图标到缓存
func DBSaveIconCache(url string, data []byte, mimeType string) error {
	_, err := DB.Exec(
		"INSERT OR REPLACE INTO icon_cache (url, data, mime_type, created_at) VALUES (?, ?, ?, ?)",
		url, data, mimeType, time.Now().Unix(),
	)
	return err
}

// DBGetIconCache 从缓存获取图标
func DBGetIconCache(url string) ([]byte, string, bool, error) {
	var data []byte
	var mimeType string
	err := DB.QueryRow("SELECT data, mime_type FROM icon_cache WHERE url = ?", url).Scan(&data, &mimeType)
	if err == sql.ErrNoRows {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	return data, mimeType, true, nil
}

// DBCleanupIconCache 清理过期的图标缓存 (例如超过 30 天)
func DBCleanupIconCache(days int) (int64, error) {
	expirationTime := time.Now().AddDate(0, 0, -days).Unix()
	res, err := DB.Exec("DELETE FROM icon_cache WHERE created_at < ?", expirationTime)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ===== 已读状态操作 =====

// DBLoadReadState 从数据库加载已读状态到内存
func DBLoadReadState() (map[string]int64, error) {
	rows, err := DB.Query("SELECT link, read_at FROM read_state")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	state := make(map[string]int64)
	for rows.Next() {
		var link string
		var readAt int64
		if err := rows.Scan(&link, &readAt); err != nil {
			return nil, err
		}
		state[link] = readAt
	}
	return state, rows.Err()
}

// DBSaveReadState 保存单条已读状态到数据库
func DBSaveReadState(link string, readAt int64) error {
	_, err := DB.Exec(
		"INSERT OR REPLACE INTO read_state (link, read_at) VALUES (?, ?)",
		link, readAt,
	)
	return err
}

// DBSaveReadStateBatch 批量保存已读状态到数据库
func DBSaveReadStateBatch(states map[string]int64) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT OR REPLACE INTO read_state (link, read_at) VALUES (?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for link, readAt := range states {
		if _, err := stmt.Exec(link, readAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DBDeleteReadState 删除已读状态
func DBDeleteReadState(link string) error {
	_, err := DB.Exec("DELETE FROM read_state WHERE link = ?", link)
	return err
}

// DBDeleteReadStateBatch 批量删除已读状态
func DBDeleteReadStateBatch(links []string) error {
	if len(links) == 0 {
		return nil
	}
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("DELETE FROM read_state WHERE link = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, link := range links {
		if _, err := stmt.Exec(link); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DBClearReadState 清空已读状态
func DBClearReadState() error {
	_, err := DB.Exec("DELETE FROM read_state")
	return err
}

// DBDeleteReadStateOlderThan 删除指定时间之前的已读状态
func DBDeleteReadStateOlderThan(timestamp int64, excludeLinks map[string]bool) (int, error) {
	if len(excludeLinks) == 0 {
		result, err := DB.Exec("DELETE FROM read_state WHERE read_at < ?", timestamp)
		if err != nil {
			return 0, err
		}
		count, _ := result.RowsAffected()
		return int(count), nil
	}

	// 有排除列表时，需要逐条检查
	rows, err := DB.Query("SELECT link FROM read_state WHERE read_at < ?", timestamp)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var toDelete []string
	for rows.Next() {
		var link string
		if err := rows.Scan(&link); err != nil {
			return 0, err
		}
		if !excludeLinks[link] {
			toDelete = append(toDelete, link)
		}
	}

	if len(toDelete) == 0 {
		return 0, nil
	}

	return len(toDelete), DBDeleteReadStateBatch(toDelete)
}

// ===== 后处理缓存操作 =====

// DBPostProcessEntry 后处理缓存条目
type DBPostProcessEntry struct {
	Link        string
	Title       string
	NewLink     string
	PubDate     string
	ProcessedAt string
}

// DBLoadPostProcessCache 从数据库加载后处理缓存
func DBLoadPostProcessCache() (map[string]DBPostProcessEntry, error) {
	rows, err := DB.Query("SELECT link, title, new_link, pub_date, processed_at FROM postprocess_cache")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cache := make(map[string]DBPostProcessEntry)
	for rows.Next() {
		var entry DBPostProcessEntry
		var title, newLink, pubDate sql.NullString
		if err := rows.Scan(&entry.Link, &title, &newLink, &pubDate, &entry.ProcessedAt); err != nil {
			return nil, err
		}
		entry.Title = title.String
		entry.NewLink = newLink.String
		entry.PubDate = pubDate.String
		cache[entry.Link] = entry
	}
	return cache, rows.Err()
}

// DBSavePostProcessCache 保存后处理缓存到数据库
func DBSavePostProcessCache(entry DBPostProcessEntry) error {
	_, err := DB.Exec(
		"INSERT OR REPLACE INTO postprocess_cache (link, title, new_link, pub_date, processed_at) VALUES (?, ?, ?, ?, ?)",
		entry.Link, entry.Title, entry.NewLink, entry.PubDate, entry.ProcessedAt,
	)
	return err
}

// DBDeletePostProcessCache 删除后处理缓存
func DBDeletePostProcessCache(link string) error {
	_, err := DB.Exec("DELETE FROM postprocess_cache WHERE link = ?", link)
	return err
}

// DBDeletePostProcessCacheBatch 批量删除后处理缓存
func DBDeletePostProcessCacheBatch(links []string) error {
	if len(links) == 0 {
		return nil
	}
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("DELETE FROM postprocess_cache WHERE link = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, link := range links {
		if _, err := stmt.Exec(link); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DBClearPostProcessCache 清空后处理缓存
func DBClearPostProcessCache() error {
	_, err := DB.Exec("DELETE FROM postprocess_cache")
	return err
}

// ===== 条目缓存操作 =====

// DBItemsCacheEntry 条目缓存条目
type DBItemsCacheEntry struct {
	RssURL       string
	Title        string
	Link         string
	OriginalLink string
	PubDate      string
	FetchTime    string
}

// DBLoadItemsCache 从数据库加载条目缓存
func DBLoadItemsCache() (map[string][]DBItemsCacheEntry, error) {
	rows, err := DB.Query("SELECT rss_url, title, link, original_link, pub_date, fetch_time FROM items_cache ORDER BY rss_url, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cache := make(map[string][]DBItemsCacheEntry)
	for rows.Next() {
		var entry DBItemsCacheEntry
		var originalLink, pubDate, fetchTime sql.NullString
		if err := rows.Scan(&entry.RssURL, &entry.Title, &entry.Link, &originalLink, &pubDate, &fetchTime); err != nil {
			return nil, err
		}
		entry.OriginalLink = originalLink.String
		entry.PubDate = pubDate.String
		entry.FetchTime = fetchTime.String
		cache[entry.RssURL] = append(cache[entry.RssURL], entry)
	}
	return cache, rows.Err()
}

// DBLoadItemsCacheForURL 从数据库加载指定URL的条目缓存
func DBLoadItemsCacheForURL(rssURL string) ([]DBItemsCacheEntry, error) {
	rows, err := DB.Query("SELECT rss_url, title, link, original_link, pub_date, fetch_time FROM items_cache WHERE rss_url = ? ORDER BY id", rssURL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []DBItemsCacheEntry
	for rows.Next() {
		var entry DBItemsCacheEntry
		var originalLink, pubDate, fetchTime sql.NullString
		if err := rows.Scan(&entry.RssURL, &entry.Title, &entry.Link, &originalLink, &pubDate, &fetchTime); err != nil {
			return nil, err
		}
		entry.OriginalLink = originalLink.String
		entry.PubDate = pubDate.String
		entry.FetchTime = fetchTime.String
		items = append(items, entry)
	}
	return items, rows.Err()
}

// DBSaveItemsCache 保存指定URL的条目缓存到数据库（会先清除该URL的旧缓存）
func DBSaveItemsCache(rssURL string, items []DBItemsCacheEntry) error {
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 先删除该URL的旧缓存
	if _, err := tx.Exec("DELETE FROM items_cache WHERE rss_url = ?", rssURL); err != nil {
		return err
	}

	// 插入新缓存
	stmt, err := tx.Prepare("INSERT INTO items_cache (rss_url, title, link, original_link, pub_date, fetch_time) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, item := range items {
		if _, err := stmt.Exec(item.RssURL, item.Title, item.Link, item.OriginalLink, item.PubDate, item.FetchTime); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// DBDeleteItemsCacheForURL 删除指定URL的条目缓存
func DBDeleteItemsCacheForURL(rssURL string) error {
	_, err := DB.Exec("DELETE FROM items_cache WHERE rss_url = ?", rssURL)
	return err
}

// DBDeleteItemsCacheForURLs 批量删除指定URL的条目缓存
func DBDeleteItemsCacheForURLs(urls []string) error {
	if len(urls) == 0 {
		return nil
	}
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("DELETE FROM items_cache WHERE rss_url = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, url := range urls {
		if _, err := stmt.Exec(url); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DBClearItemsCache 清空条目缓存
func DBClearItemsCache() error {
	_, err := DB.Exec("DELETE FROM items_cache")
	return err
}

// DBGetItemsCacheURLs 获取所有有缓存的URL列表
func DBGetItemsCacheURLs() ([]string, error) {
	rows, err := DB.Query("SELECT DISTINCT rss_url FROM items_cache")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var urls []string
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			return nil, err
		}
		urls = append(urls, url)
	}
	return urls, rows.Err()
}
