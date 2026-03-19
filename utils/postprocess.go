package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"feedora/globals"
	"feedora/models"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// PostProcessResponse AI后处理响应结构
type PostProcessResponse struct {
	Title   string `json:"title,omitempty"`
	Link    string `json:"link,omitempty"`
	PubDate string `json:"pubDate,omitempty"`
}

// postProcessResult 后处理结果
type postProcessResult struct {
	index     int
	item      models.Item
	fromCache bool
	err       error
}

// PostProcessItems 对Feed条目进行后处理（并行处理）
func PostProcessItems(items []models.Item, rssURL string) []models.Item {
	config := getPostProcessConfig(rssURL)
	if config == nil || !config.Enabled {
		return items
	}

	// 记录开始日志
	mode := config.GetMode()
	modifyFields := []string{}
	if config.ModifyTitle {
		modifyFields = append(modifyFields, "标题")
	}
	if config.ModifyLink {
		modifyFields = append(modifyFields, "链接")
	}
	if config.ModifyPubDate {
		modifyFields = append(modifyFields, "发布时间")
	}
	log.Printf("[后处理开始] 源 [%s] | 模式: %s | 待处理: %d 条 | 修改字段: %s",
		rssURL, mode, len(items), strings.Join(modifyFields, ", "))

	// 获取并发数（复用AI分类的并发配置）
	concurrency := globals.RssUrls.AIClassify.GetConcurrency()
	if concurrency > len(items) {
		concurrency = len(items)
	}
	if concurrency <= 0 {
		concurrency = 1
	}

	// 创建工作通道和结果通道
	jobChan := make(chan struct {
		index int
		item  models.Item
	}, len(items))
	resultChan := make(chan postProcessResult, len(items))

	// 启动worker goroutines
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobChan {
				result := postProcessResult{
					index: job.index,
					item:  job.item,
				}

				// 获取原始链接作为缓存 key（优先使用 OriginalLink，如果没有则使用 Link）
				originalLink := job.item.OriginalLink
				if originalLink == "" {
					originalLink = job.item.Link
				}

				// 先检查缓存
				cacheEntry, cached := GetPostProcessCache(originalLink)
				if cached {
					// 使用缓存结果
					if config.ModifyTitle && cacheEntry.Title != "" {
						result.item.Title = cacheEntry.Title
					}
					if config.ModifyLink && cacheEntry.Link != "" {
						// 保存原始链接（如果还没有的话）
						if result.item.OriginalLink == "" {
							result.item.OriginalLink = result.item.Link
						}
						result.item.Link = cacheEntry.Link
					}
					if config.ModifyPubDate && cacheEntry.PubDate != "" {
						result.item.PubDate = cacheEntry.PubDate
					}
					result.fromCache = true
				} else {
					// 没有缓存，执行后处理（带重试机制）
					aiConfig := globals.RssUrls.AIClassify
					maxRetries := aiConfig.GetRetryCount()
					retryWait := time.Duration(aiConfig.GetRetryWait()) * time.Second

					var processedItem models.Item
					var lastErr error

					for attempt := 1; attempt <= maxRetries; attempt++ {
						if config.GetMode() == "script" {
							processedItem, lastErr = processItemWithScript(job.item, config)
						} else {
							processedItem, lastErr = processItemWithAI(job.item, config)
						}

						if lastErr == nil {
							break
						}

						if attempt < maxRetries {
							retryType := "失败"
							if lastErr != nil && (strings.Contains(strings.ToLower(lastErr.Error()), "timeout") || strings.Contains(lastErr.Error(), "deadline exceeded")) {
								retryType = "超时"
							}
							log.Printf("[后处理重试] 条目 [%s]: 第 %d/%d 次尝试%s: %v，%d秒后重试...",
								job.item.Title, attempt, maxRetries-1, retryType, lastErr, int(retryWait.Seconds()))
							time.Sleep(retryWait)
						}
					}

					if lastErr != nil {
						result.err = lastErr
						log.Printf("[后处理失败] 条目 [%s]: 已尝试 %d 次，最终失败: %v", job.item.Title, maxRetries, lastErr)
						// 失败后不存入缓存，下次源更新时将重新处理
					} else {
						// 如果后处理会修改 Link，先保存原始链接
						if config.ModifyLink && processedItem.Link != job.item.Link {
							processedItem.OriginalLink = job.item.Link
						}
						result.item = processedItem

						// 记录成功处理的详细信息
						changes := []string{}
						if config.ModifyTitle && processedItem.Title != job.item.Title {
							changes = append(changes, fmt.Sprintf("标题: [%s] -> [%s]", truncateString(job.item.Title, 20), truncateString(processedItem.Title, 20)))
						}
						if config.ModifyLink && processedItem.Link != job.item.Link {
							changes = append(changes, fmt.Sprintf("链接已修改"))
						}
						if config.ModifyPubDate && processedItem.PubDate != job.item.PubDate {
							changes = append(changes, fmt.Sprintf("时间: [%s] -> [%s]", job.item.PubDate, processedItem.PubDate))
						}
						if len(changes) > 0 {
							log.Printf("[后处理成功] 条目 [%s] | %s", truncateString(job.item.Title, 30), strings.Join(changes, ", "))
						}

						// 成功后存入缓存（使用原始链接作为 key）
						entry := models.PostProcessCacheEntry{
							ProcessedAt: time.Now().Format("2006-01-02 15:04:05"),
						}
						if config.ModifyTitle {
							entry.Title = processedItem.Title
						}
						if config.ModifyLink {
							entry.Link = processedItem.Link
						}
						if config.ModifyPubDate {
							entry.PubDate = processedItem.PubDate
						}
						SetPostProcessCache(originalLink, entry)
					}
				}

				resultChan <- result
			}
		}()
	}

	// 发送所有任务
	for i, item := range items {
		jobChan <- struct {
			index int
			item  models.Item
		}{index: i, item: item}
	}
	close(jobChan)

	// 等待所有worker完成
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// 收集结果并按原始顺序排序
	results := make([]postProcessResult, 0, len(items))
	for result := range resultChan {
		results = append(results, result)
	}

	// 按索引排序保持原顺序
	sort.Slice(results, func(i, j int) bool {
		return results[i].index < results[j].index
	})

	// 构建最终结果
	processedItems := make([]models.Item, 0, len(items))
	cacheHits := 0
	newItems := 0
	failedItems := 0

	for _, result := range results {
		if result.fromCache {
			cacheHits++
		} else if result.err != nil {
			failedItems++
		} else {
			newItems++
		}
		processedItems = append(processedItems, result.item)
	}

	// 展示统计（无论是否有新处理都展示，方便追踪）
	log.Printf("[后处理完成] 源 [%s] | 新处理: %d 篇, 失败: %d 篇, 缓存命中: %d 篇 | 总计: %d 篇",
		rssURL, newItems, failedItems, cacheHits, len(items))

	return processedItems
}

// truncateString 截断字符串，添加省略号
func truncateString(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// getPostProcessConfig 获取指定URL的后处理配置
func getPostProcessConfig(rssURL string) *models.PostProcessConfig {
	for _, source := range globals.RssUrls.Sources {
		if source.URL == rssURL {
			return source.PostProcess
		}
	}
	return nil
}

// ShouldPostProcess 检查是否应该启用后处理
func ShouldPostProcess(rssURL string) bool {
	config := getPostProcessConfig(rssURL)
	return config != nil && config.Enabled
}

// processItemWithAI 使用AI处理条目
func processItemWithAI(item models.Item, config *models.PostProcessConfig) (models.Item, error) {
	aiConfig := globals.RssUrls.AIClassify
	if aiConfig.APIKey == "" {
		return item, fmt.Errorf("AI API Key未配置")
	}

	// 构建提示词
	prompt := config.Prompt
	if prompt == "" {
		prompt = "请对以下RSS条目进行处理。"
	}
	prompt += "\n\n输出要求（必须全部满足）：" +
		"\n1. 只返回一个 JSON 对象，不要返回 markdown、代码块、解释或额外文本。" +
		"\n2. 仅允许出现这三个字段：title、link、pubDate。" +
		"\n3. 未修改的字段也要保留原值，不要留空，不要省略。" +
		"\n4. `title`、`link`、`pubDate` 的值都必须是字符串。" +
		"\n5. 输出格式必须是：{\"title\":\"...\",\"link\":\"...\",\"pubDate\":\"...\"}。"

	// 构建条目内容
	itemJSON, _ := json.Marshal(map[string]string{
		"title":   item.Title,
		"link":    item.Link,
		"pubDate": item.PubDate,
	})

	// 构建请求
	reqBody := ChatRequest{
		Model: aiConfig.GetModel(),
		Messages: []ChatMessage{
			{Role: "system", Content: prompt},
			{Role: "user", Content: string(itemJSON)},
		},
		Temperature: aiConfig.GetTemperature(),
		MaxTokens:   aiConfig.GetMaxTokens(),
	}

	jsonMode := aiConfig.GetJSONMode()
	maybeEnableJSONObjectResponseFormat(&reqBody, jsonMode, prompt, string(itemJSON))

	client := &http.Client{
		Timeout: time.Duration(aiConfig.GetTimeout()) * time.Second,
	}
	chatResp, err := sendChatCompletion(client, aiConfig.GetAPIBase(), aiConfig.APIKey, jsonMode, reqBody)
	if err != nil {
		return item, err
	}

	// 解析后处理结果
	responseContent := chatResp.Choices[0].Message.Content
	responseContent = stripCodeFences(responseContent)

	var postProcessResp PostProcessResponse
	if err := json.Unmarshal([]byte(responseContent), &postProcessResp); err != nil {
		return item, fmt.Errorf("解析后处理响应失败: %w, 内容: %s", err, responseContent)
	}

	// 应用处理结果
	processedItem := item
	if config.ModifyTitle && postProcessResp.Title != "" {
		processedItem.Title = postProcessResp.Title
	}
	if config.ModifyLink && postProcessResp.Link != "" {
		processedItem.Link = postProcessResp.Link
	}
	if config.ModifyPubDate && postProcessResp.PubDate != "" {
		processedItem.PubDate = postProcessResp.PubDate
	}

	return processedItem, nil
}

// processItemWithScript 使用脚本处理条目
func processItemWithScript(item models.Item, config *models.PostProcessConfig) (models.Item, error) {
	// 创建超时 context（复用 AI 的超时配置）
	timeout := time.Duration(globals.RssUrls.AIClassify.GetTimeout()) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 将条目转换为JSON
	itemJSON, err := json.Marshal(map[string]string{
		"title":       item.Title,
		"link":        item.Link,
		"pubDate":     item.PubDate,
		"source":      item.Source,
		"description": item.Description,
	})
	if err != nil {
		return item, fmt.Errorf("序列化条目失败: %w", err)
	}

	var cmd *exec.Cmd

	// 优先使用内联脚本内容
	if config.ScriptContent != "" {
		// 使用 bash -c 直接执行脚本内容
		cmd = exec.CommandContext(ctx, "bash", "-c", config.ScriptContent)
	} else if config.ScriptPath != "" {
		// 使用脚本文件
		cmd = exec.CommandContext(ctx, config.ScriptPath)
	} else {
		return item, fmt.Errorf("脚本内容或脚本路径未配置")
	}

	cmd.Stdin = bytes.NewReader(itemJSON)

	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return item, fmt.Errorf("脚本执行超时（超过 %v）", timeout)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return item, fmt.Errorf("脚本执行失败: %s, stderr: %s", err, string(exitErr.Stderr))
		}
		return item, fmt.Errorf("脚本执行失败: %w", err)
	}

	// 解析脚本输出
	var postProcessResp PostProcessResponse
	if err := json.Unmarshal(output, &postProcessResp); err != nil {
		return item, fmt.Errorf("解析脚本输出失败: %w, 输出: %s", err, string(output))
	}

	// 应用处理结果
	processedItem := item
	if config.ModifyTitle && postProcessResp.Title != "" {
		processedItem.Title = postProcessResp.Title
	}
	if config.ModifyLink && postProcessResp.Link != "" {
		processedItem.Link = postProcessResp.Link
	}
	if config.ModifyPubDate && postProcessResp.PubDate != "" {
		processedItem.PubDate = postProcessResp.PubDate
	}

	return processedItem, nil
}
