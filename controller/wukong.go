package controller

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// 透传专用 HTTP 客户端配置
var (
	chatStreamClient     *http.Client
	chatStreamClientOnce sync.Once
)

// getChatStreamClient 获取透传专用的 HTTP 客户端
// 配置长超时时间以支持 LLM 长时间生成
func getChatStreamClient() *http.Client {
	chatStreamClientOnce.Do(func() {
		// 从环境变量读取超时配置，默认 30 分钟
		timeoutMinutes := common.GetEnvOrDefault("WUKONG_PROXY_TIMEOUT_MINUTES", 30)
		timeout := time.Duration(timeoutMinutes) * time.Minute

		chatStreamClient = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second, // 连接超时
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second, // 首字节响应超时
				// 不设置 DisableKeepAlives，保持连接复用
			},
		}
	})
	return chatStreamClient
}

// WukongModelInfo 定义 wukong 格式的模型信息
type WukongModelInfo struct {
	Description        string `json:"description"`
	DisplayName        string `json:"displayName"`
	ShortName          string `json:"shortName"`
	IsNew              *bool  `json:"isNew,omitempty"`
	IsLegacyModel      *bool  `json:"isLegacyModel,omitempty"`
	Priority           *int   `json:"priority,omitempty"`
	ModelGroupPriority *int   `json:"modelGroupPriority,omitempty"`
}

// WukongGetBalance 获取用户余额
// GET /usage/api/balance
func WukongGetBalance(c *gin.Context) {
	tokenId := c.GetInt("token_id")

	// 获取 token 信息
	token, err := model.GetTokenById(tokenId)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"error":   "获取令牌信息失败",
		})
		return
	}

	// 计算余额
	remainQuota := token.RemainQuota
	unlimited := token.UnlimitedQuota

	remainAmount := float64(remainQuota) / common.QuotaPerUnit

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"name":          token.Name,
			"remain_quota":  remainQuota,
			"remain_amount": remainAmount,
			"unlimited":     unlimited,
			"expired_time":  token.ExpiredTime,
			"status":        token.Status,
			"status_text":   "enabled",
		},
	})
}

// WukongGetModels 获取模型列表（wukong 格式）
// GET /usage/api/get-models
func WukongGetModels(c *gin.Context) {
	userId := c.GetInt("id")
	userGroup, _ := model.GetUserGroup(userId, false)

	// 获取 token 分组
	tokenGroup := common.GetContextKeyString(c, constant.ContextKeyTokenGroup)
	group := userGroup
	if tokenGroup != "" && tokenGroup != "auto" {
		group = tokenGroup
	}

	// 获取分组可用模型
	var models []string
	if tokenGroup == "auto" {
		for _, autoGroup := range service.GetUserAutoGroup(userGroup) {
			groupModels := model.GetGroupEnabledModels(autoGroup)
			for _, g := range groupModels {
				if !common.StringsContains(models, g) {
					models = append(models, g)
				}
			}
		}
	} else {
		models = model.GetGroupEnabledModels(group)
	}

	// 转换为 wukong 格式
	result := make(map[string]WukongModelInfo)
	priority := 1

	for _, modelName := range models {
		p := priority
		result[modelName] = WukongModelInfo{
			Description: modelName,
			DisplayName: modelName,
			ShortName:   modelName,
			Priority:    &p,
		}
		priority++
	}

	c.JSON(http.StatusOK, result)
}

// WukongChatStream 转发请求到后端服务并计费
// POST /chat-stream
// 流程:
// 1. 验证用户 token (已通过 TokenAuth 中间件)
// 2. 预扣费额度
// 3. 透传请求到 CLIProxyAPIPlus
// 4. 流式返回响应并解析 usage
// 5. 根据实际 token 用量调整计费
func WukongChatStream(c *gin.Context) {
	startTime := time.Now()

	// 获取用户信息 (由 TokenAuth 中间件设置)
	userId := c.GetInt("id")
	tokenId := c.GetInt("token_id")
	tokenName := c.GetString("token_name")
	tokenKey := c.GetString("token_key")
	tokenUnlimited := c.GetBool("token_unlimited_quota")
	usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)

	// 读取请求体
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": types.OpenAIError{
				Message: "读取请求体失败: " + err.Error(),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	// 解析请求获取模型名
	var chatReq dto.GeneralOpenAIRequest
	if err := json.Unmarshal(body, &chatReq); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": types.OpenAIError{
				Message: "解析请求体失败: " + err.Error(),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	modelName := chatReq.Model
	if modelName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": types.OpenAIError{
				Message: "model 字段是必须的",
				Type:    "invalid_request_error",
			},
		})
		return
	}

	logger.LogInfo(c, fmt.Sprintf("ChatStream 请求: model=%s, userId=%d, group=%s", modelName, userId, usingGroup))

	// 获取模型倍率和分组倍率
	modelRatio, _, _ := ratio_setting.GetModelRatio(modelName)
	completionRatio := ratio_setting.GetCompletionRatio(modelName)
	groupRatio := ratio_setting.GetGroupRatio(usingGroup)

	// 估算预扣费额度 (基于估计的 token 数)
	estimatedTokens := 1000 // 默认估计值
	preConsumeQuota := int(float64(estimatedTokens) * modelRatio * groupRatio)
	if preConsumeQuota < 1 {
		preConsumeQuota = 1
	}

	// 检查用户额度
	userQuota, err := model.GetUserQuota(userId, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": types.OpenAIError{
				Message: "获取用户额度失败",
				Type:    "server_error",
			},
		})
		return
	}

	if userQuota <= 0 {
		c.JSON(http.StatusForbidden, gin.H{
			"error": types.OpenAIError{
				Message: fmt.Sprintf("余额不足，剩余额度: %s", logger.FormatQuota(userQuota)),
				Type:    "insufficient_quota",
			},
		})
		return
	}

	// 预扣费
	actualPreConsume := 0
	if userQuota > preConsumeQuota {
		actualPreConsume = preConsumeQuota
		if !tokenUnlimited {
			err = model.DecreaseTokenQuota(tokenId, tokenKey, actualPreConsume)
			if err != nil {
				logger.LogError(c, "预扣费 token 额度失败: "+err.Error())
			}
		}
		err = model.DecreaseUserQuota(userId, actualPreConsume)
		if err != nil {
			logger.LogError(c, "预扣费用户额度失败: "+err.Error())
		}
		logger.LogInfo(c, fmt.Sprintf("预扣费: %s, 模型: %s", logger.FormatQuota(actualPreConsume), modelName))
	}

	// 转发请求到 CLIProxyAPIPlus
	targetURL := common.GetEnvOrDefaultString("WUKONG_PROXY_URL", "")
	if targetURL == "" {
		host := common.GetEnvOrDefaultString("WUKONG_PROXY_HOST", "127.0.0.1")
		port := common.GetEnvOrDefaultString("WUKONG_PROXY_PORT", "8319")
		targetURL = "http://" + host + ":" + port + "/chat-stream"
	}

	// 创建转发请求
	proxyReq, err := http.NewRequest("POST", targetURL, bytes.NewReader(body))
	if err != nil {
		returnPreConsumedQuota(userId, tokenId, tokenKey, tokenUnlimited, actualPreConsume)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": types.OpenAIError{
				Message: "创建请求失败",
				Type:    "server_error",
			},
		})
		return
	}

	// 复制请求头
	for key, values := range c.Request.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}
	proxyReq.Header.Set("Content-Type", "application/json")

	// 设置上游认证
	proxyAPIKey := common.GetEnvOrDefaultString("WUKONG_API_KEY", "sk-cliproxy-internal-for-newapi")
	if proxyAPIKey != "" {
		proxyReq.Header.Set("Authorization", "Bearer "+proxyAPIKey)
	}

	// 发送请求 - 使用透传专用客户端（长超时）
	client := getChatStreamClient()
	resp, err := client.Do(proxyReq)
	if err != nil {
		returnPreConsumedQuota(userId, tokenId, tokenKey, tokenUnlimited, actualPreConsume)
		c.JSON(http.StatusBadGateway, gin.H{
			"error": types.OpenAIError{
				Message: "后端服务请求失败: " + err.Error(),
				Type:    "upstream_error",
			},
		})
		return
	}
	defer resp.Body.Close()

	// 设置响应头
	c.Status(resp.StatusCode)
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}

	// 流式传输响应并解析 usage
	streamResult := streamAndParseUsage(c, resp.Body)

	// 计算实际消耗
	useTimeSeconds := int(time.Since(startTime).Seconds())
	promptTokens := streamResult.Usage.PromptTokens
	completionTokens := streamResult.Usage.CompletionTokens

	// 如果没有解析到 usage，则根据内容长度估算 token（中文约 0.6 token/char，英文约 0.25 token/char）
	if promptTokens == 0 && completionTokens == 0 {
		var estimatedCompletionTokens int
		if len(streamResult.ResponseContent) > 0 {
			// 估算：假设平均 0.4 token/char（中英文混合）
			estimatedCompletionTokens = int(float64(len(streamResult.ResponseContent)) * 0.4)
		} else if streamResult.RawBytesCount > 0 {
			// Fallback: 使用原始字节数估算（假设 JSON 包装占 50%）
			estimatedCompletionTokens = int(float64(streamResult.RawBytesCount) * 0.2)
		}
		if estimatedCompletionTokens < 1 && streamResult.RawBytesCount > 100 {
			estimatedCompletionTokens = 1
		}
		if estimatedCompletionTokens > 0 {
			completionTokens = estimatedCompletionTokens
			// 估算 prompt tokens（基于请求 body 大小）
			promptTokens = int(float64(len(body)) * 0.3)
			if promptTokens < 1 {
				promptTokens = 1
			}
			common.SysLog(fmt.Sprintf("Token 估算: prompt=%d, completion=%d (内容长度=%d, 原始字节=%d)",
				promptTokens, completionTokens, len(streamResult.ResponseContent), streamResult.RawBytesCount))
		}
	}

	totalTokens := promptTokens + completionTokens

	var actualQuota int
	if totalTokens > 0 {
		// 计算实际额度: (prompt + completion * completionRatio) * modelRatio * groupRatio
		actualQuotaDecimal := decimal.NewFromInt(int64(promptTokens)).
			Add(decimal.NewFromInt(int64(completionTokens)).Mul(decimal.NewFromFloat(completionRatio))).
			Mul(decimal.NewFromFloat(modelRatio)).
			Mul(decimal.NewFromFloat(groupRatio))
		actualQuota = int(actualQuotaDecimal.Round(0).IntPart())
		if actualQuota < 1 && (promptTokens > 0 || completionTokens > 0) {
			actualQuota = 1
		}
	}

	// 计算差额并调整
	quotaDelta := actualQuota - actualPreConsume

	if quotaDelta != 0 {
		if quotaDelta > 0 {
			// 需要补扣
			if !tokenUnlimited {
				model.DecreaseTokenQuota(tokenId, tokenKey, quotaDelta)
			}
			model.DecreaseUserQuota(userId, quotaDelta)
			logger.LogInfo(c, fmt.Sprintf("补扣费: %s (实际: %s, 预扣: %s)",
				logger.FormatQuota(quotaDelta), logger.FormatQuota(actualQuota), logger.FormatQuota(actualPreConsume)))
		} else {
			// 需要返还
			if !tokenUnlimited {
				model.IncreaseTokenQuota(tokenId, tokenKey, -quotaDelta)
			}
			model.IncreaseUserQuota(userId, -quotaDelta, false)
			logger.LogInfo(c, fmt.Sprintf("返还额度: %s (实际: %s, 预扣: %s)",
				logger.FormatQuota(-quotaDelta), logger.FormatQuota(actualQuota), logger.FormatQuota(actualPreConsume)))
		}
	}

	// 记录消费日志
	if totalTokens > 0 {
		model.UpdateUserUsedQuotaAndRequestCount(userId, actualQuota)
		logContent := fmt.Sprintf("模型倍率 %.2f，补全倍率 %.2f，分组倍率 %.2f",
			modelRatio, completionRatio, groupRatio)
		model.RecordConsumeLog(c, userId, model.RecordConsumeLogParams{
			ChannelId:        0, // 透传模式无渠道ID
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			ModelName:        modelName,
			TokenName:        tokenName,
			Quota:            actualQuota,
			Content:          logContent,
			TokenId:          tokenId,
			UseTimeSeconds:   useTimeSeconds,
			IsStream:         true,
			Group:            usingGroup,
		})
	}
}

// StreamResult 包含流式传输的结果
type StreamResult struct {
	Usage           dto.Usage
	ResponseContent string // 累积的响应内容，用于估算 token
	RawBytesCount   int    // 原始字节数，用于 fallback 估算
}

// streamAndParseUsage 流式传输响应并解析 usage 信息
// 支持多种 SSE 格式和非标准 usage 位置
// 同时累积响应内容用于 token 估算（当无 usage 信息时）
func streamAndParseUsage(c *gin.Context, body io.Reader) StreamResult {
	var result StreamResult
	var contentBuilder strings.Builder
	reader := bufio.NewReader(body)

	debugEnabled := common.GetEnvOrDefaultBool("WUKONG_DEBUG", false)

	// 计数器
	lineCount := 0
	dataLineCount := 0

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// 处理最后可能没有换行的数据
				if len(line) > 0 {
					c.Writer.Write([]byte(line))
					c.Writer.Flush()
					result.RawBytesCount += len(line)
					// 尝试解析
					parseUsageFromLine(line, &result.Usage)
					extractContentFromLine(line, &contentBuilder)
				}
				break
			}
			common.SysError("读取响应流失败: " + err.Error())
			break
		}

		lineCount++
		result.RawBytesCount += len(line)

		// 写入响应
		c.Writer.Write([]byte(line))
		c.Writer.Flush()

		// 去除行尾换行符进行解析
		trimmedLine := strings.TrimRight(line, "\r\n")

		// 调试日志 - 记录所有非空行的前200字符
		if debugEnabled && len(trimmedLine) > 0 {
			logLine := trimmedLine
			if len(logLine) > 200 {
				logLine = logLine[:200] + "..."
			}
			common.SysLog(fmt.Sprintf("DEBUG SSE [%d]: %s", lineCount, logLine))
		}

		// 解析 SSE 数据
		if strings.HasPrefix(trimmedLine, "data: ") {
			dataLineCount++
			data := strings.TrimPrefix(trimmedLine, "data: ")
			if data == "[DONE]" {
				continue
			}

			// 尝试解析 usage
			parseUsageFromData(data, &result.Usage)
			// 提取内容用于 token 估算
			extractContentFromData(data, &contentBuilder)
		} else if strings.HasPrefix(trimmedLine, "data:") {
			// 处理 "data:" 后直接跟数据的情况（无空格）
			dataLineCount++
			data := strings.TrimPrefix(trimmedLine, "data:")
			if data != "[DONE]" && data != "" {
				parseUsageFromData(data, &result.Usage)
				extractContentFromData(data, &contentBuilder)
			}
		} else if strings.Contains(trimmedLine, `"usage"`) {
			// 有些响应可能直接在行中包含 usage，不在 data: 前缀后
			parseUsageFromData(trimmedLine, &result.Usage)
		} else if len(trimmedLine) > 0 && !strings.HasPrefix(trimmedLine, ":") && !strings.HasPrefix(trimmedLine, "event:") {
			// 非标准 SSE：可能是纯文本流或其他格式
			// 尝试作为 JSON 解析
			if strings.HasPrefix(trimmedLine, "{") {
				parseUsageFromData(trimmedLine, &result.Usage)
				extractContentFromData(trimmedLine, &contentBuilder)
			} else {
				// 可能是纯文本响应，直接累积
				contentBuilder.WriteString(trimmedLine)
			}
		}
	}

	result.ResponseContent = contentBuilder.String()

	// 记录解析结果用于调试
	common.SysLog(fmt.Sprintf("SSE 解析完成: 总行数=%d, data行数=%d, 内容长度=%d, 原始字节=%d",
		lineCount, dataLineCount, len(result.ResponseContent), result.RawBytesCount))

	if result.Usage.PromptTokens == 0 && result.Usage.CompletionTokens == 0 {
		if len(result.ResponseContent) == 0 && result.RawBytesCount > 0 {
			common.SysLog("警告: 未能提取内容，将使用原始字节数估算")
		}
	} else {
		common.SysLog(fmt.Sprintf("解析到 usage: prompt=%d, completion=%d, total=%d",
			result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens))
	}

	return result
}

// extractContentFromData 从 SSE 数据中提取文本内容
// 支持多种响应格式
func extractContentFromData(data string, builder *strings.Builder) {
	// 格式 1: OpenAI 标准流式格式 - choices[].delta.content
	var streamResp1 struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &streamResp1); err == nil {
		for _, choice := range streamResp1.Choices {
			if choice.Delta.Content != "" {
				builder.WriteString(choice.Delta.Content)
				return
			}
		}
	}

	// 格式 2: 非流式格式 - choices[].message.content
	var streamResp2 struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &streamResp2); err == nil {
		for _, choice := range streamResp2.Choices {
			if choice.Message.Content != "" {
				builder.WriteString(choice.Message.Content)
				return
			}
		}
	}

	// 格式 3: 直接 content 字段
	var streamResp3 struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(data), &streamResp3); err == nil {
		if streamResp3.Content != "" {
			builder.WriteString(streamResp3.Content)
			return
		}
	}

	// 格式 4: text 字段
	var streamResp4 struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(data), &streamResp4); err == nil {
		if streamResp4.Text != "" {
			builder.WriteString(streamResp4.Text)
			return
		}
	}

	// 格式 5: response 字段
	var streamResp5 struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal([]byte(data), &streamResp5); err == nil {
		if streamResp5.Response != "" {
			builder.WriteString(streamResp5.Response)
			return
		}
	}

	// 格式 6: data.content 嵌套格式
	var streamResp6 struct {
		Data struct {
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(data), &streamResp6); err == nil {
		if streamResp6.Data.Content != "" {
			builder.WriteString(streamResp6.Data.Content)
			return
		}
	}
}

// extractContentFromLine 从行中提取内容
func extractContentFromLine(line string, builder *strings.Builder) {
	start := strings.Index(line, "{")
	if start != -1 {
		extractContentFromData(line[start:], builder)
	}
}

// min 返回两个整数中较小的一个
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// parseUsageFromData 从 JSON 数据中解析 usage
func parseUsageFromData(data string, usage *dto.Usage) {
	// 尝试标准 OpenAI 流式响应格式
	var streamResp dto.ChatCompletionsStreamResponse
	if err := json.Unmarshal([]byte(data), &streamResp); err == nil {
		if streamResp.Usage != nil {
			*usage = *streamResp.Usage
			return
		}
	}

	// 尝试简单响应格式
	var simpleResp dto.ChatCompletionsStreamResponseSimple
	if err := json.Unmarshal([]byte(data), &simpleResp); err == nil {
		if simpleResp.Usage != nil {
			*usage = *simpleResp.Usage
			return
		}
	}

	// 尝试直接解析 usage 字段
	var usageWrapper struct {
		Usage *dto.Usage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &usageWrapper); err == nil {
		if usageWrapper.Usage != nil {
			*usage = *usageWrapper.Usage
			return
		}
	}

	// 尝试解析嵌套的 usage 对象（有些 API 返回 {"data": {"usage": {...}}} 格式）
	var nestedResp struct {
		Data struct {
			Usage *dto.Usage `json:"usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(data), &nestedResp); err == nil {
		if nestedResp.Data.Usage != nil {
			*usage = *nestedResp.Data.Usage
			return
		}
	}
}

// parseUsageFromLine 从整行文本中尝试提取 usage 信息
func parseUsageFromLine(line string, usage *dto.Usage) {
	// 查找 JSON 起始位置
	start := strings.Index(line, "{")
	if start == -1 {
		return
	}

	// 尝试从 JSON 开始位置解析
	jsonData := line[start:]
	parseUsageFromData(jsonData, usage)
}

// returnPreConsumedQuota 返还预扣费额度
func returnPreConsumedQuota(userId, tokenId int, tokenKey string, tokenUnlimited bool, preConsumed int) {
	if preConsumed <= 0 {
		return
	}
	if !tokenUnlimited {
		model.IncreaseTokenQuota(tokenId, tokenKey, preConsumed)
	}
	model.IncreaseUserQuota(userId, preConsumed, false)
}
