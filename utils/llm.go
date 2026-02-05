package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"feedora/globals"
	"feedora/models"
	"sort"
	"strings"
	"sync"
	"time"
)

// ClassifyResponse AI分类响应结构
// ClassifyResponse AI分类响应结构
type ClassifyResponse struct {
	Category   string  `json:"category"`
}

// CheckBatchResponse 批量检查响应结构 (Map: index -> valid class)
type CheckBatchResponse struct {
	Results map[string]bool `json:"results"`
}

// BatchClassifyResponse 批量AI分类响应结构
type BatchClassifyResponse struct {
	Results map[string]string `json:"results"`
}

// LLMClient 大模型客户端
type LLMClient struct {
	config models.AIClassifyConfig
	client *http.Client
}

// NewLLMClient 创建新的LLM客户端
func NewLLMClient(config models.AIClassifyConfig) *LLMClient {
	return &LLMClient{
		config: config,
		client: &http.Client{
			Timeout: time.Duration(config.GetTimeout()) * time.Second,
		},
	}
}

// ChatMessage 聊天消息结构
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest 聊天请求结构
type ChatRequest struct {
	Model          string         `json:"model"`
	Messages       []ChatMessage  `json:"messages"`
	Temperature    float64        `json:"temperature,omitempty"`
	MaxTokens      int            `json:"max_tokens,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// ResponseFormat 响应格式
type ResponseFormat struct {
	Type string `json:"type"`
}

// ChatResponse 聊天响应结构
type ChatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// ClassifyBatchItems 对一批RSS文章进行AI分类
func (c *LLMClient) ClassifyBatchItems(items map[int]models.Item, strategy *models.ClassifyStrategy, categories []models.Category) (*BatchClassifyResponse, error) {
	if len(items) == 0 {
		return &BatchClassifyResponse{Results: make(map[string]string)}, nil
	}

	// 构建批量文章内容
	var contentBuilder strings.Builder
	contentBuilder.WriteString("请对以下文章进行分类。\n")
	contentBuilder.WriteString("返回一个JSON对象，键为文章的索引ID(string)，值为最匹配的类别ID(string)。\n")
	contentBuilder.WriteString("文章列表：\n\n")

	// 为了保持顺序稳定，我们按索引排序处理
	indices := make([]int, 0, len(items))
	for idx := range items {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	for _, idx := range indices {
		item := items[idx]
		contentBuilder.WriteString(fmt.Sprintf("--- 文章 ID: %d ---\n", idx))
		contentBuilder.WriteString(buildItemContent(item))
		contentBuilder.WriteString("\n\n")
	}

	content := contentBuilder.String()

	// 构建类别信息
	var categoryInfo strings.Builder
	categoryInfo.WriteString("可用类别：\n")
	for _, cat := range categories {
		categoryInfo.WriteString(fmt.Sprintf("- %s (%s): %s\n", cat.ID, cat.Name, cat.Description))
	}

	// 获取系统提示词
	systemPrompt := c.config.GetSystemPrompt()
	if strategy != nil && strategy.CustomPrompt != "" {
		systemPrompt = strategy.CustomPrompt
	}

	// 强制JSON模式提示
	systemPrompt += "\n\n你必须返回严格的JSON格式。不要包含markdown标记。"

	// 构建请求
	systemContent := systemPrompt + "\n\n" + categoryInfo.String()
	reqBody := ChatRequest{
		Model: c.config.GetModel(),
		Messages: []ChatMessage{
			{Role: "system", Content: systemContent},
			{Role: "user", Content: content},
		},
		Temperature: c.config.GetTemperature(),
		MaxTokens:   c.config.GetMaxTokens() * 2, // 批量处理适当增加token
		ResponseFormat: &ResponseFormat{
			Type: "json_object",
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	// 发送请求
	apiURL := fmt.Sprintf("%s/chat/completions", strings.TrimSuffix(c.config.GetAPIBase(), "/"))
	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.config.APIKey))

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w (Body: %s)", err, string(body))
	}

	if chatResp.Error != nil {
		return nil, fmt.Errorf("API错误: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("API未返回有效响应")
	}

	// 解析批量分类结果
	responseContent := chatResp.Choices[0].Message.Content
	return parseBatchClassifyResponse(responseContent)
}

// parseBatchClassifyResponse 解析批量分类响应
func parseBatchClassifyResponse(content string) (*BatchClassifyResponse, error) {
	jsonStr := extractJSON(content)
	if jsonStr == "" {
		// 尝试直接解析
		jsonStr = content
	}

	// 尝试解析 {"results": {"0": "cat1", "1": "cat2"}}
	var standardResp BatchClassifyResponse
	if err := json.Unmarshal([]byte(jsonStr), &standardResp); err == nil && len(standardResp.Results) > 0 {
		return &standardResp, nil
	}
	
	// 尝试解析直接的 Map 结构 {"0": "cat1", "1": "cat2"}
	var mapResp map[string]string
	if err := json.Unmarshal([]byte(jsonStr), &mapResp); err == nil {
		return &BatchClassifyResponse{Results: mapResp}, nil
	}

	// 兼容：尝试解析旧的结构（如果模型还是返回了复杂对象）
	var oldStructResp struct{
		Results map[string]ClassifyResponse `json:"results"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &oldStructResp); err == nil && len(oldStructResp.Results) > 0 {
		results := make(map[string]string)
		for k, v := range oldStructResp.Results {
			results[k] = v.Category
		}
		return &BatchClassifyResponse{Results: results}, nil
	}

	return nil, fmt.Errorf("无法解析批量分类响应: %s", content)
}

// ClassifyItemWithCategories 对RSS文章进行AI分类
// categories: 可用的类别列表
// keywordOnly: 如果为true，只进行关键词过滤，不调用AI
func (c *LLMClient) ClassifyItemWithCategories(item models.Item, strategy *models.ClassifyStrategy, categories []models.Category, keywordOnly bool) (*ClassifyResponse, error) {
	// 先检查关键词过滤
	if strategy != nil {
		// 检查保留关键词
		hasKeepKeyword := false
		for _, keyword := range strategy.KeepKeywords {
			if containsKeyword(item.Title, keyword) || containsKeyword(item.Description, keyword) {
				hasKeepKeyword = true
				break
			}
		}

		// 白名单模式：仅保留包含保留关键词的文章
		if strategy.IsWhitelistMode() {
			if hasKeepKeyword {
				return &ClassifyResponse{
					Category:   "_keep",
				}, nil
			}
			// 白名单模式下，不包含保留关键词的文章全部过滤
			return &ClassifyResponse{
				Category:   "_filtered",
			}, nil
		}

		// 非白名单模式：包含保留关键词的文章直接保留
		if hasKeepKeyword {
			return &ClassifyResponse{
				Category:   "_keep",
			}, nil
		}

		// 检查过滤关键词
		for _, keyword := range strategy.FilterKeywords {
			if containsKeyword(item.Title, keyword) || containsKeyword(item.Description, keyword) {
				return &ClassifyResponse{
					Category:   "_filtered",
				}, nil
			}
		}
	}

	// 如果只需要关键词过滤，不调用AI
	if keywordOnly {
		return &ClassifyResponse{
			Category:   "",
		}, nil
	}

	// 构建文章内容
	content := buildItemContent(item)

	// 构建类别信息
	var categoryInfo strings.Builder
	categoryInfo.WriteString("可用类别：\n")
	for _, cat := range categories {
		categoryInfo.WriteString(fmt.Sprintf("- %s (%s): %s\n", cat.ID, cat.Name, cat.Description))
	}

	// 获取系统提示词
	systemPrompt := c.config.GetSystemPrompt()
	if strategy != nil && strategy.CustomPrompt != "" {
		systemPrompt = strategy.CustomPrompt
	}

	// 构建请求
	systemContent := systemPrompt + "\n\n" + categoryInfo.String()
	reqBody := ChatRequest{
		Model: c.config.GetModel(),
		Messages: []ChatMessage{
			{Role: "system", Content: systemContent},
			{Role: "user", Content: content},
		},
		Temperature: c.config.GetTemperature(),
		MaxTokens:   c.config.GetMaxTokens(),
	}

	// 某些 API (如 OpenAI, Ark) 要求开启 json_object 时，提示词中必须包含 "json" 字样
	// 且只有当提示词明确要求 JSON 时，开启此模式才有意义
	if strings.Contains(strings.ToLower(systemContent), "json") || strings.Contains(strings.ToLower(content), "json") {
		reqBody.ResponseFormat = &ResponseFormat{
			Type: "json_object",
		}
	} else {
		// 如果不要求 JSON 格式，则显式设置为 nil 或不设置，以支持返回纯文本 ID
		reqBody.ResponseFormat = nil
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	// 发送请求
	apiURL := fmt.Sprintf("%s/chat/completions", strings.TrimSuffix(c.config.GetAPIBase(), "/"))
	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.config.APIKey))

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	if chatResp.Error != nil {
		return nil, fmt.Errorf("API错误: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("API未返回有效响应")
	}

	// 解析分类结果
	responseContent := chatResp.Choices[0].Message.Content
	return parseClassifyResponse(responseContent)
}

// buildItemContent 构建文章内容用于分类
func buildItemContent(item models.Item) string {
	var content strings.Builder
	content.WriteString("标题: ")
	content.WriteString(item.Title)
	content.WriteString("\n")

	if item.Description != "" {
		// 移除HTML标签
		desc := stripHTML(item.Description)
		// 限制长度（使用配置的最大描述长度）
		maxDescLen := globals.RssUrls.AIClassify.GetMaxDescLength()
		if len(desc) > maxDescLen {
			desc = desc[:maxDescLen] + "..."
		}
		content.WriteString("内容: ")
		content.WriteString(desc)
	}

	return content.String()
}

// stripHTML 移除HTML标签
func stripHTML(html string) string {
	// 移除HTML标签
	re := regexp.MustCompile(`<[^>]*>`)
	text := re.ReplaceAllString(html, " ")
	// 清理多余空白
	re = regexp.MustCompile(`\s+`)
	text = re.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

// containsKeyword 检查文本是否包含关键词（不区分大小写）
func containsKeyword(text, keyword string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(keyword))
}

// parseClassifyResponse 解析分类响应
func parseClassifyResponse(content string) (*ClassifyResponse, error) {
	// 尝试从中提取 JSON
	jsonStr := extractJSON(content)
	
	// 如果提取到了 JSON，尝试解析
	if jsonStr != "" {
		var resp ClassifyResponse
		if err := json.Unmarshal([]byte(jsonStr), &resp); err == nil {
			if resp.Category != "" {
				return &resp, nil
			}
		}
		
		// 尝试解析数组格式（某些模型可能返回数组）
		var respArray []ClassifyResponse
		if err := json.Unmarshal([]byte(jsonStr), &respArray); err == nil && len(respArray) > 0 {
			if respArray[0].Category != "" {
				return &respArray[0], nil
			}
		}
	}

	// 如果 JSON 解析失败，或者提取不到 JSON，降级到纯文本处理
	// 移除可能存在的引号和空白
	content = strings.Trim(strings.TrimSpace(content), "\"`")
	if content != "" && len(content) < 100 {
		return &ClassifyResponse{
			Category:   content,
		}, nil
	}

	return nil, fmt.Errorf("无法从响应中解析有效的分类信息: %s", content)
}

// extractJSON 尝试从文本中提取 JSON 部分
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	
	// 如果本身就是 JSON
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		return s
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		return s
	}

	// 查找 ```json ... ```
	re := regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")
	if matches := re.FindStringSubmatch(s); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}

	// 查找第一个 { 和最后一个 }
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start != -1 && end != -1 && end > start {
		return s[start : end+1]
	}

	// 查找第一个 [ 和最后一个 ]
	start = strings.Index(s, "[")
	end = strings.LastIndex(s, "]")
	if start != -1 && end != -1 && end > start {
		return s[start : end+1]
	}

	return ""
}

// stripCodeFences 移除代码块标记 (保留此函数以兼容其他地方的调用，但建议内部使用 extractJSON)
func stripCodeFences(s string) string {
	extracted := extractJSON(s)
	if extracted != "" {
		return extracted
	}
	
	s = strings.TrimSpace(s)
	// 移除 ```json 和 ``` 标记
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
	}
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}

// classifyResult AI分类结果
type classifyResult struct {
	index      int
	item       models.Item
	category   string
	fromCache  bool
	err        error
}

// ClassifyItems 对Feed中的Items进行AI分类（并行处理 + 批量请求）
// 返回带有分类信息的Items
func ClassifyItems(items []models.Item, rssURL string) []models.Item {
	config := globals.RssUrls.AIClassify
	strategy := getClassifyStrategy(rssURL)

	// 检查是否只使用关键词过滤（不使用AI）
	useAI := ShouldUseAI(rssURL)
	keywordOnly := !useAI

	client := NewLLMClient(config)

	// 获取可用的类别列表
	categories := config.GetCategories(&globals.RssUrls)
	// 如果源配置了绑定类别，只使用绑定的类别
	if strategy != nil && len(strategy.BoundCategories) > 0 {
		boundCats := make([]models.Category, 0)
		boundMap := make(map[string]bool)
		for _, id := range strategy.BoundCategories {
			boundMap[id] = true
		}
		for _, cat := range categories {
			if boundMap[cat.ID] {
				boundCats = append(boundCats, cat)
			}
		}
		if len(boundCats) > 0 {
			categories = boundCats
		} else {
			log.Printf("[分类警告] 源 [%s]: 配置的绑定类别未匹配到任何有效类别，将使用所有类别", rssURL)
		}
	}
	
	// 检查是否有可用的类别
	if len(categories) == 0 {
		log.Printf("[分类错误] 源 [%s]: 没有可用的分类类别，跳过分类", rssURL)
		return items
	}

	// 最终结果列表（保持顺序）
	finalItems := make([]models.Item, len(items))
	copy(finalItems, items)

	// 待处理任务列表
	type classifyTask struct {
		index int
		item  models.Item
	}
	pendingTasks := make([]classifyTask, 0)

	// 1. 先检查缓存，同时检查关键词过滤
	cacheHits := 0
	keywordHits := 0
	globals.ClassifyCacheLock.RLock()
	for i, item := range items {
		// 先检查缓存
		cacheEntry, cached := globals.ClassifyCache[item.Link]
		if cached && cacheEntry.Category != "" {
			finalItems[i].Category = cacheEntry.Category
			cacheHits++
			continue
		}

		// 检查关键词过滤（即便启用了AI，关键词过滤也优先进行以节省资源）
		if strategy != nil && (strategy.IsKeywordEnabled() || strategy.IsWhitelistMode()) {
			// 使用 ClassifyItemWithCategories 来统一处理关键词过滤逻辑（传 keywordOnly=true）
			resp, _ := client.ClassifyItemWithCategories(item, strategy, categories, true)
			if resp != nil && (resp.Category == "_filtered" || resp.Category == "_keep") {
				finalItems[i].Category = resp.Category
				keywordHits++
				continue
			}
		}

		// 缓存和关键词都没搞定，交给后续处理
		pendingTasks = append(pendingTasks, classifyTask{index: i, item: item})
	}
	globals.ClassifyCacheLock.RUnlock()

	// 更新统计
	if keywordHits > 0 {
		log.Printf("[关键词过滤] 源 [%s]: 关键词匹配 %d 篇", rssURL, keywordHits)
	}

	// 如果没有待处理任务，直接返回
	if len(pendingTasks) == 0 {
		return applyFiltersAndReturn(finalItems, strategy, rssURL, 0, 0, cacheHits)
	}

	// 2. 只有关键词过滤的情况，不需要AI，直接在本地处理
	if keywordOnly {
		for _, task := range pendingTasks {
			resp, _ := client.ClassifyItemWithCategories(task.item, strategy, categories, true)
			finalItems[task.index].Category = resp.Category
		}
		return applyFiltersAndReturn(finalItems, strategy, rssURL, len(pendingTasks), 0, cacheHits)
	}

	// 3. AI 批量处理
	// 每次批量处理的数量 (Batch Size)
	batchSize := config.GetBatchSize()
	
	// 计算需要的批次数量
	numBatches := (len(pendingTasks) + batchSize - 1) / batchSize
	
	// 并发控制通道 (控制同时进行的 HTTP 请求数)
	concurrency := config.GetConcurrency()
	if concurrency <= 0 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	
	var wg sync.WaitGroup
	var mu sync.Mutex // 保护 finalItems 和统计数据
	
	newItems := 0
	failedItems := 0
	
	// 分批处理
	for i := 0; i < numBatches; i++ {
		start := i * batchSize
		end := start + batchSize
		if end > len(pendingTasks) {
			end = len(pendingTasks)
		}
		
		batchTasks := pendingTasks[start:end]
		
		wg.Add(1)
		sem <- struct{}{} // 获取信号量
		
		go func(tasks []classifyTask) {
			defer wg.Done()
			defer func() { <-sem }() // 释放信号量
			
			// 构建当前批次的 map
			batchItemsMap := make(map[int]models.Item)
			for _, t := range tasks {
				batchItemsMap[t.index] = t.item
			}
			
			// 分类
			var resp *BatchClassifyResponse
			var err error
			
			// 重试机制
			maxRetries := config.GetRetryCount()
			retryWait := time.Duration(config.GetRetryWait()) * time.Second
			for attempt := 1; attempt <= maxRetries; attempt++ {
				resp, err = client.ClassifyBatchItems(batchItemsMap, strategy, categories)
				if err == nil {
					break
				}
				if attempt < maxRetries {
					retryType := "失败"
					if strings.Contains(strings.ToLower(err.Error()), "timeout") || strings.Contains(err.Error(), "deadline exceeded") {
						retryType = "超时"
					}
					log.Printf("[重试] 批量分类请求%s (第 %d/%d 次重试): %v", retryType, attempt, maxRetries-1, err)
					time.Sleep(retryWait)
				}
			}
			
			mu.Lock()
			defer mu.Unlock()
			
			if err != nil {
				log.Printf("[分类失败] 批量请求失败 (包含 %d 篇文章): %v", len(tasks), err)
				failedItems += len(tasks)
				return
			}
			
			// 处理响应
			for _, t := range tasks {
				idxStr := fmt.Sprintf("%d", t.index)
				
				// 查找结果 (先尝试 string key)
				categoryID, ok := resp.Results[idxStr]
				if !ok {
					// 某些模型可能会返回不纯的 key, 尝试遍历查找（如果 key 包含 index）
					// 这里简单处理：如果找不到，记为失败
					failedItems++
					continue
				}
				
				// 应用结果
				finalItems[t.index].Category = categoryID
				newItems++
				
				if categoryID != "" && categoryID != "_keep" && categoryID != "_filtered" {
					log.Printf("[分类完成] 文章 [%s]: %s", finalItems[t.index].Title, categoryID)
				}
				
				// 存入缓存
				globals.ClassifyCacheLock.Lock()
				globals.ClassifyCache[finalItems[t.index].Link] = models.ClassifyCacheEntry{
					Category: categoryID,
				}
				globals.ClassifyCacheLock.Unlock()
			}
			
			// 标记数据已变更
			MarkDataChanged()
			
		}(batchTasks)
	}
	
	wg.Wait()
	
	return applyFiltersAndReturn(finalItems, strategy, rssURL, newItems, failedItems, cacheHits)
}

// applyFiltersAndReturn 应用后续过滤并返回
func applyFiltersAndReturn(items []models.Item, strategy *models.ClassifyStrategy, rssURL string, newItems, failedItems, cacheHits int) []models.Item {
	// 统计输出
	if newItems > 0 || failedItems > 0 {
		log.Printf("[分类统计] 源 [%s]: 新分类 %d 篇，失败 %d 篇 | 缓存命中 %d 篇", 
			rssURL, newItems, failedItems, cacheHits)
	}

	// 1. 首先过滤掉被关键词标记为 _filtered 的条目
	filteredItems := make([]models.Item, 0, len(items))
	keywordFilteredCount := 0
	for _, item := range items {
		if item.Category == "_filtered" {
			keywordFilteredCount++
			continue
		}
		filteredItems = append(filteredItems, item)
	}
	if keywordFilteredCount > 0 {
		log.Printf("[关键词过滤] 源 [%s]: 过滤掉 %d 篇文章", rssURL, keywordFilteredCount)
	}

	// 2. 应用类别黑白名单过滤
	if strategy != nil && (len(strategy.CategoryWhitelist) > 0 || len(strategy.CategoryBlacklist) > 0) {
		filteredItems = applyCategoryFilter(filteredItems, strategy)
	}

	// 应用脚本规则过滤
	if strategy != nil && strategy.IsScriptFilterEnabled() && strategy.ScriptFilterContent != "" {
		beforeScriptCount := len(filteredItems)
		var err error
		filteredItems, err = ApplyScriptFilter(filteredItems, strategy.ScriptFilterContent, rssURL)
		if err != nil {
			log.Printf("[脚本规则过滤失败] 源 [%s]: %v，保留原始条目", rssURL, err)
		} else {
			filteredByScript := beforeScriptCount - len(filteredItems)
			if filteredByScript > 0 {
				log.Printf("[脚本规则过滤] 源 [%s]: 过滤前 %d 篇，过滤后 %d 篇，过滤 %d 篇", 
					rssURL, beforeScriptCount, len(filteredItems), filteredByScript)
			}
		}
	}
	
	return filteredItems
}

// applyCategoryFilter 应用类别黑白名单过滤
func applyCategoryFilter(items []models.Item, strategy *models.ClassifyStrategy) []models.Item {
	if strategy == nil {
		return items
	}

	// 构建白名单和黑名单映射
	whitelistMap := make(map[string]bool)
	blacklistMap := make(map[string]bool)
	for _, cat := range strategy.CategoryWhitelist {
		whitelistMap[cat] = true
	}
	for _, cat := range strategy.CategoryBlacklist {
		blacklistMap[cat] = true
	}

	filtered := make([]models.Item, 0, len(items))
	for _, item := range items {
		// 如果是被关键词标记为 _keep 的，直接保留，跳过类别过滤
		if item.Category == "_keep" {
			filtered = append(filtered, item)
			continue
		}

		// 如果有白名单，只保留白名单中的类别
		if len(whitelistMap) > 0 {
			if whitelistMap[item.Category] {
				filtered = append(filtered, item)
			}
			continue
		}
		
		// 如果有黑名单，过滤掉黑名单中的类别
		if len(blacklistMap) > 0 {
			if !blacklistMap[item.Category] {
				filtered = append(filtered, item)
			}
			continue
		}
		
		filtered = append(filtered, item)
	}

	if len(items) != len(filtered) {
		log.Printf("[类别过滤] 过滤前 %d 篇，过滤后 %d 篇", len(items), len(filtered))
	}

	return filtered
}

// getClassifyStrategy 获取指定URL的分类策略
func getClassifyStrategy(rssURL string) *models.ClassifyStrategy {
	for _, source := range globals.RssUrls.Sources {
		if source.URL == rssURL {
			return source.Classify
		}
	}
	return nil
}

// ShouldFilter 检查是否应该启用过滤（关键词或AI或脚本）
func ShouldFilter(rssURL string) bool {
	config := globals.RssUrls.AIClassify

	// 获取该源的特定策略
	strategy := getClassifyStrategy(rssURL)
	if strategy == nil {
		return false
	}

	// 检查是否启用关键词过滤
	if strategy.IsKeywordEnabled() {
		return true
	}

	// 检查是否启用脚本规则过滤
	if strategy.IsScriptFilterEnabled() {
		return true
	}

	// 检查是否启用AI分类（需要全局AI分类启用且有API Key）
	if config.Enabled && config.APIKey != "" && strategy.IsAIEnabled() {
		return true
	}

	return false
}

// ShouldUseAI 检查是否应该使用AI分类
func ShouldUseAI(rssURL string) bool {
	config := globals.RssUrls.AIClassify
	if !config.Enabled || config.APIKey == "" {
		return false
	}

	strategy := getClassifyStrategy(rssURL)
	if strategy == nil {
		return false
	}

	return strategy.IsAIEnabled()
}

// ApplyScriptFilter 应用脚本规则过滤
// 脚本通过 stdin 接收所有条目的 JSON 数组，返回过滤后的条目 JSON 数组
// 输入格式：[{"title":"标题1","link":"链接1","pubDate":"时间1",...}, ...]
// 输出格式：[{"title":"标题1","link":"链接1","pubDate":"时间1",...}, ...]
func ApplyScriptFilter(items []models.Item, scriptContent string, rssURL string) ([]models.Item, error) {
	if len(items) == 0 {
		return items, nil
	}

	// 创建超时 context（复用 AI 的超时配置）
	timeout := time.Duration(globals.RssUrls.AIClassify.GetTimeout()) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 将所有条目转换为 JSON 数组
	itemsJSON, err := json.Marshal(items)
	if err != nil {
		return items, fmt.Errorf("序列化条目失败: %w", err)
	}

	// 使用 bash -c 直接执行脚本内容
	cmd := exec.CommandContext(ctx, "bash", "-c", scriptContent)
	cmd.Stdin = bytes.NewReader(itemsJSON)

	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return items, fmt.Errorf("脚本执行超时（超过 %v）", timeout)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return items, fmt.Errorf("脚本执行失败: %s, stderr: %s", err, string(exitErr.Stderr))
		}
		return items, fmt.Errorf("脚本执行失败: %w", err)
	}

	// 如果输出为空，表示过滤掉了所有条目
	trimmedOutput := strings.TrimSpace(string(output))
	if trimmedOutput == "" {
		return []models.Item{}, nil
	}

	// 解析脚本输出（应该是过滤后的条目数组）
	var filteredItems []models.Item
	if err := json.Unmarshal(output, &filteredItems); err != nil {
		// 尝试解析是否是 JSON Lines 格式（每行一个 JSON 对象）
		lines := strings.Split(trimmedOutput, "\n")
		var itemsL []models.Item
		validJSONLines := true
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var item models.Item
			if err := json.Unmarshal([]byte(line), &item); err != nil {
				validJSONLines = false
				break
			}
			itemsL = append(itemsL, item)
		}
		
		if validJSONLines && len(itemsL) > 0 {
			return itemsL, nil
		}
		return items, fmt.Errorf("解析脚本输出失败: %w, 输出: %s", err, string(output))
	}

	return filteredItems, nil
}
