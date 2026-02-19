package models

import (
	"encoding/json"
	"os"
)

func ParseConf() (Config, error) {
	var conf Config
	data, err := os.ReadFile("config.json")
	if err != nil {
		return conf, err
	}
	// 解析JSON数据到Config结构体
	err = json.Unmarshal(data, &conf)

	return conf, err
}

// Category AI分类类别
type Category struct {
	// 类别ID（唯一标识）
	ID string `json:"id"`
	// 类别名称（显示用）
	Name string `json:"name"`
	// 类别描述（用于AI分类时的参考）
	Description string `json:"description,omitempty"`
	// 类别颜色（用于前端显示）
	Color string `json:"color,omitempty"`
}

// CategoryPackage 分类类别包
type CategoryPackage struct {
	// 包ID
	ID string `json:"id"`
	// 包名称
	Name string `json:"name"`
	// 包内类别列表
	Categories []Category `json:"categories,omitempty"`
}

// AIClassifyConfig AI分类配置
type AIClassifyConfig struct {
	// 是否全局启用AI分类
	Enabled bool `json:"enabled"`
	// API Key
	APIKey string `json:"apiKey"`
	// API Base URL (兼容 OpenAI 格式的 API)
	APIBase string `json:"apiBase,omitempty"`
	// 模型名称
	Model string `json:"model,omitempty"`
	// 系统提示词
	SystemPrompt string `json:"systemPrompt,omitempty"`
	// 最大 token 数
	MaxTokens int `json:"maxTokens,omitempty"`
	// Temperature
	Temperature float64 `json:"temperature,omitempty"`
	// 请求超时时间（秒）
	Timeout int `json:"timeout,omitempty"`
	// 并发数，同时进行的AI分类请求数量
	Concurrency int `json:"concurrency,omitempty"`
	// 最大描述长度（发送给AI的内容描述截断长度，默认2000）
	MaxDescLength int `json:"maxDescLength,omitempty"`
	// 批量处理数量 (Batch Size)，默认 5
	BatchSize int `json:"batchSize,omitempty"`
	// 重试次数，默认 3
	RetryCount int `json:"retryCount,omitempty"`
	// 重试等待时间（秒），默认 2
	RetryWait int `json:"retryWait,omitempty"`
	// 分类类别包列表 (新版)
	CategoryPackages []CategoryPackage `json:"categoryPackages,omitempty"`
}

// GetAPIBase 获取 API Base URL，默认为火山引擎
func (c AIClassifyConfig) GetAPIBase() string {
	if c.APIBase == "" {
		return "https://ark.cn-beijing.volces.com/api/v3"
	}
	return c.APIBase
}

// GetModel 获取模型名称，默认为 doubao-seed-1.8
func (c AIClassifyConfig) GetModel() string {
	if c.Model == "" {
		return "doubao-seed-1.8"
	}
	return c.Model
}

// GetSystemPrompt 获取系统提示词
func (c AIClassifyConfig) GetSystemPrompt() string {
	return c.SystemPrompt
}

// GetCategories 获取分类类别列表，如果未配置则使用全局分类
func (c AIClassifyConfig) GetCategories(config *Config) []Category {
	if len(c.CategoryPackages) == 0 {
		return config.GetCategories()
	}
	allCats := make([]Category, 0)
	for _, pkg := range c.CategoryPackages {
		allCats = append(allCats, pkg.Categories...)
	}
	return allCats
}

// GetMaxTokens 获取最大 token 数，默认为 500
func (c AIClassifyConfig) GetMaxTokens() int {
	if c.MaxTokens == 0 {
		return 500
	}
	return c.MaxTokens
}

// GetTemperature 获取 temperature，默认为 0.1
func (c AIClassifyConfig) GetTemperature() float64 {
	if c.Temperature == 0 {
		return 0.1
	}
	return c.Temperature
}

// GetTimeout 获取超时时间，默认为 30 秒
func (c AIClassifyConfig) GetTimeout() int {
	if c.Timeout == 0 {
		return 30
	}
	return c.Timeout
}

// GetConcurrency 获取并发数，默认为 5
func (c AIClassifyConfig) GetConcurrency() int {
	if c.Concurrency <= 0 {
		return 5
	}
	return c.Concurrency
}

// GetMaxDescLength 获取最大描述长度，默认为 2000
func (c AIClassifyConfig) GetMaxDescLength() int {
	if c.MaxDescLength <= 0 {
		return 2000
	}
	return c.MaxDescLength
}

// GetBatchSize 获取批量处理数量，默认为 5
func (c AIClassifyConfig) GetBatchSize() int {
	if c.BatchSize <= 0 {
		return 5
	}
	return c.BatchSize
}

// GetRetryCount 获取重试次数，默认为 3
func (c AIClassifyConfig) GetRetryCount() int {
	if c.RetryCount < 0 {
		return 0
	}
	if c.RetryCount == 0 {
		return 3
	}
	return c.RetryCount
}

// GetRetryWait 获取重试等待时间（秒），默认为 2
func (c AIClassifyConfig) GetRetryWait() int {
	if c.RetryWait <= 0 {
		return 2
	}
	return c.RetryWait
}

// FetchSchedule 抓取计划规则
type FetchSchedule struct {
	StartTime    string `json:"startTime"`    // HH:mm:ss
	EndTime      string `json:"endTime"`      // HH:mm:ss
	BaseRefresh  int    `json:"baseRefresh"`  // 基准频率 (分钟)
	DefaultCount int    `json:"defaultCount"` // 默认次数
}

// ClassifyStrategy 分类策略配置
type ClassifyStrategy struct {
	// 是否启用关键词过滤
	KeywordEnabled *bool `json:"keywordEnabled,omitempty"`
	// 是否启用AI分类
	AIEnabled *bool `json:"aiEnabled,omitempty"`
	// 过滤关键词（包含这些关键词的文章将被过滤）
	FilterKeywords []string `json:"filterKeywords,omitempty"`
	// 保留关键词（包含这些关键词的文章将被保留，优先级高于过滤）
	KeepKeywords []string `json:"keepKeywords,omitempty"`
	// 白名单模式：启用后仅保留包含保留关键词的文章（其他全部过滤）
	WhitelistMode *bool `json:"whitelistMode,omitempty"`
	// 是否启用脚本规则过滤
	ScriptFilterEnabled *bool `json:"scriptFilterEnabled,omitempty"`
	// 脚本规则过滤的脚本内容（Shell 脚本，通过 stdin 接收条目的 JSON 数组）
	ScriptFilterContent string `json:"scriptFilterContent,omitempty"`
	// 绑定的类别ID列表（发送给AI时仅包含这些类别，为空表示全选）
	BoundCategories []string `json:"boundCategories,omitempty"`
	// 类别黑名单（这些类别的文章将被过滤）
	CategoryBlacklist []string `json:"categoryBlacklist,omitempty"`
	// 类别白名单（仅保留这些类别的文章，优先级高于黑名单）
	CategoryWhitelist []string `json:"categoryWhitelist,omitempty"`
	// 自定义AI提示词（覆盖全局）
	CustomPrompt string `json:"customPrompt,omitempty"`
}

// IsKeywordEnabled 检查是否启用关键词过滤
func (f ClassifyStrategy) IsKeywordEnabled() bool {
	if f.KeywordEnabled != nil {
		return *f.KeywordEnabled
	}
	return false
}

// IsAIEnabled 检查是否启用AI分类
func (f ClassifyStrategy) IsAIEnabled() bool {
	if f.AIEnabled != nil {
		return *f.AIEnabled
	}
	return false
}

// IsWhitelistMode 检查是否启用白名单模式
func (f ClassifyStrategy) IsWhitelistMode() bool {
	if f.WhitelistMode != nil {
		return *f.WhitelistMode
	}
	return false
}

// IsScriptFilterEnabled 检查是否启用脚本规则过滤
func (f ClassifyStrategy) IsScriptFilterEnabled() bool {
	if f.ScriptFilterEnabled != nil {
		return *f.ScriptFilterEnabled
	}
	return false
}

// PostProcessConfig 后处理配置
type PostProcessConfig struct {
	// 是否启用后处理
	Enabled bool `json:"enabled"`
	// 处理模式: "ai" 或 "script"
	Mode string `json:"mode,omitempty"`
	// AI模式的提示词
	Prompt string `json:"prompt,omitempty"`
	// 脚本模式的脚本路径（二选一）
	ScriptPath string `json:"scriptPath,omitempty"`
	// 脚本模式的脚本内容（二选一，优先级高于ScriptPath）
	ScriptContent string `json:"scriptContent,omitempty"`
	// 是否修改标题
	ModifyTitle bool `json:"modifyTitle,omitempty"`
	// 是否修改链接
	ModifyLink bool `json:"modifyLink,omitempty"`
	// 是否修改发布时间
	ModifyPubDate bool `json:"modifyPubDate,omitempty"`
}

// GetMode 获取处理模式，默认为ai
func (p PostProcessConfig) GetMode() string {
	if p.Mode == "" {
		return "ai"
	}
	return p.Mode
}

// Source 表示单个RSS订阅源
type Source struct {
	// RSS源的URL（唯一标识）
	URL string `json:"url"`
	// 自定义名称
	Name string `json:"name,omitempty"`
	// 自定义图标URL
	Icon string `json:"icon,omitempty"`
	// AI分类策略
	Classify *ClassifyStrategy `json:"classify,omitempty"`
	// 忽略原始发布时间：启用后将忽略RSS源自带的发布时间，使用首次出现时间代替
	IgnoreOriginalPubDate bool `json:"ignoreOriginalPubDate,omitempty"`
	// 榜单模式：启用后每次获取的条目都按原始排列顺序展示，不读取缓存中的发布时间
	RankingMode bool `json:"rankingMode,omitempty"`
	// 最大读取条目数，超过此数量的条目将不会被加载（0或不设置表示不限制）
	MaxItems int `json:"maxItems,omitempty"`
	// 缓存条目数：0或不设置表示自动缓存所有过滤后的条目，>0表示缓存指定数量，-1表示禁用缓存
	CacheItems int `json:"cacheItems,omitempty"`
	// 后处理配置
	PostProcess *PostProcessConfig `json:"postProcess,omitempty"`
	// 自定义刷新次数，与时段规则中的基准频率相乘
	RefreshCount int `json:"refreshCount,omitempty"`
	// 是否在条目后显示发布时间（如"1小时前"）
	ShowPubDate bool `json:"showPubDate,omitempty"`
	// 是否显示分类标签
	ShowCategory bool `json:"showCategory,omitempty"`
}

// HasAIClassify 判断该源是否启用了AI分类
func (s Source) HasAIClassify() bool {
	return s.Classify != nil && s.Classify.IsAIEnabled()
}

// FolderEntry 文件夹绑定条目（一个订阅源+可选的类别绑定）
type FolderEntry struct {
	// 订阅源URL（普通订阅源条目使用）
	SourceURL string `json:"sourceUrl,omitempty"`
	// 分类包ID（分类包条目使用，会自动包含该分类包对应的所有订阅源）
	CategoryPackageId string `json:"categoryPackageId,omitempty"`
	// 绑定的类别ID列表 (多选支持)
	Categories []string `json:"categories,omitempty"`
	// 是否隐藏源名称（默认显示，true为隐藏）
	HideSource bool `json:"hideSource,omitempty"`
}

// Folder 表示文件夹配置
type Folder struct {
	// 文件夹ID（唯一标识，自动生成）
	ID string `json:"id"`
	// 文件夹名称
	Name string `json:"name"`
	// 自定义图标URL
	Icon string `json:"icon,omitempty"`
	// 文件夹绑定的条目列表（订阅源+类别绑定）
	Entries []FolderEntry `json:"entries,omitempty"`
	// 是否在条目后显示发布时间
	ShowPubDate bool `json:"showPubDate,omitempty"`
	// 是否显示分类标签
	ShowCategory bool `json:"showCategory,omitempty"`
	// 是否显示源名称标签
	ShowSource bool `json:"showSource,omitempty"`
}

// LayoutItem 布局项（可以是订阅源或文件夹）
type LayoutItem struct {
	// 类型: "source" 或 "folder"
	Type string `json:"type"`
	// 订阅源URL（type为source时）
	SourceURL string `json:"sourceUrl,omitempty"`
	// 文件夹ID（type为folder时）
	FolderID string `json:"folderId,omitempty"`
}

// LayoutGroup 分组布局配置
type LayoutGroup struct {
	// 分组ID（唯一标识）
	ID string `json:"id"`
	// 分组名称
	Name string `json:"name"`
	// 分组包含的布局项列表（按显示顺序排列）
	Items []LayoutItem `json:"items,omitempty"`
}

// Config 主配置结构
type Config struct {
	// 订阅源列表
	Sources []Source `json:"sources,omitempty"`
	// 文件夹列表
	Folders []Folder `json:"folders,omitempty"`
	// 分组布局列表
	LayoutGroups []LayoutGroup `json:"layoutGroups,omitempty"`
	// 抓取计划规则列表
	Schedules []FetchSchedule `json:"schedules,omitempty"`
	// 夜间模式起始时间
	NightStartTime string `json:"nightStartTime,omitempty"`
	// 夜间模式结束时间
	NightEndTime string `json:"nightEndTime,omitempty"`
	// 是否启用夜间模式 (手动覆盖)
	DarkMode bool `json:"darkMode,omitempty"`
	// 新条目序号加粗高亮颜色
	BoldColor string `json:"boldColor,omitempty"`
	// Settings password
	Password string `json:"password,omitempty"`
	// Session duration in hours (default: 24)
	SessionDuration int `json:"sessionDuration,omitempty"`
	// AI分类配置
	AIClassify AIClassifyConfig `json:"aiClassify,omitempty"`
	// 默认选中的分组ID（可选，默认为第一个分组）
	DefaultGroup string `json:"defaultGroup,omitempty"`
	// 全局分类类别列表
	Categories []Category `json:"categories,omitempty"`
}

// GetAllUrls 获取所有RSS源URL
func (c Config) GetAllUrls() []string {
	urls := make([]string, 0)
	for _, source := range c.Sources {
		if source.URL != "" {
			urls = append(urls, source.URL)
		}
	}
	return urls
}

// GetSourceByURL 根据URL获取订阅源
func (c Config) GetSourceByURL(url string) *Source {
	for i := range c.Sources {
		if c.Sources[i].URL == url {
			return &c.Sources[i]
		}
	}
	return nil
}

// GetFolderByID 根据ID获取文件夹
func (c Config) GetFolderByID(id string) *Folder {
	for i := range c.Folders {
		if c.Folders[i].ID == id {
			return &c.Folders[i]
		}
	}
	return nil
}

// GetLayoutGroupByID 根据ID获取分组布局
func (c Config) GetLayoutGroupByID(id string) *LayoutGroup {
	for i := range c.LayoutGroups {
		if c.LayoutGroups[i].ID == id {
			return &c.LayoutGroups[i]
		}
	}
	return nil
}

// GetAllAIClassifySources 获取所有启用AI分类的订阅源
func (c Config) GetAllAIClassifySources() []Source {
	sources := make([]Source, 0)
	for _, source := range c.Sources {
		if source.HasAIClassify() {
			sources = append(sources, source)
		}
	}
	return sources
}

// GetSourcesByPackageId 获取某个分类包对应的所有订阅源
// 返回所有 BoundCategories 中包含该分类包内类别的订阅源
func (c Config) GetSourcesByPackageId(packageId string) []Source {
	sources := make([]Source, 0)
	
	// 找到对应的分类包
	var pkg *CategoryPackage
	if c.AIClassify.CategoryPackages != nil {
		for i := range c.AIClassify.CategoryPackages {
			if c.AIClassify.CategoryPackages[i].ID == packageId {
				pkg = &c.AIClassify.CategoryPackages[i]
				break
			}
		}
	}
	
	if pkg == nil || len(pkg.Categories) == 0 {
		return sources
	}
	
	// 获取该分类包的所有类别ID
	categoryIds := make(map[string]bool)
	for _, cat := range pkg.Categories {
		categoryIds[cat.ID] = true
	}
	
	// 查找所有 BoundCategories 中包含该分类包类别的源
	for _, source := range c.Sources {
		if !source.HasAIClassify() {
			continue
		}
		if source.Classify == nil || len(source.Classify.BoundCategories) == 0 {
			continue
		}
		
		// 检查源的 BoundCategories 是否与分类包的类别有交集
		for _, catId := range source.Classify.BoundCategories {
			if categoryIds[catId] {
				sources = append(sources, source)
				break
			}
		}
	}
	
	return sources
}

// GetIncrement 获取新增的URL
func (older Config) GetIncrement(newer Config) []string {
	var (
		urlMap    = make(map[string]struct{})
		increment = make([]string, 0)
	)
	for _, item := range older.GetAllUrls() {
		urlMap[item] = struct{}{}
	}

	for _, item := range newer.GetAllUrls() {
		if _, ok := urlMap[item]; ok {
			continue
		}
		increment = append(increment, item)
	}

	return increment
}

// GetSessionDuration 获取会话有效期（小时），默认为 24
func (c Config) GetSessionDuration() int {
	if c.SessionDuration <= 0 {
		return 24
	}
	return c.SessionDuration
}

// GetCategories 获取全局分类类别列表
func (c Config) GetCategories() []Category {
	return c.Categories
}

// GetGroups 获取所有分组名称（按布局顺序）
func (c Config) GetGroups() []string {
	groups := make([]string, 0, len(c.LayoutGroups))
	for _, lg := range c.LayoutGroups {
		groups = append(groups, lg.Name)
	}
	if len(groups) == 0 {
		groups = append(groups, "关注")
	}
	return groups
}

// GetDefaultGroupName 获取默认分组名称
func (c Config) GetDefaultGroupName() string {
	if c.DefaultGroup != "" {
		for _, lg := range c.LayoutGroups {
			if lg.ID == c.DefaultGroup {
				return lg.Name
			}
		}
	}
	if len(c.LayoutGroups) > 0 {
		return c.LayoutGroups[0].Name
	}
	return "关注"
}
