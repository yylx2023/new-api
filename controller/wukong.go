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
	usage := streamAndParseUsage(c, resp.Body)

	// 计算实际消耗
	useTimeSeconds := int(time.Since(startTime).Seconds())
	promptTokens := usage.PromptTokens
	completionTokens := usage.CompletionTokens
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

// streamAndParseUsage 流式传输响应并解析 usage 信息
func streamAndParseUsage(c *gin.Context, body io.Reader) dto.Usage {
	var usage dto.Usage
	scanner := bufio.NewScanner(body)
	// 增大缓冲区以处理大响应
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		// 写入响应
		c.Writer.Write([]byte(line + "\n"))
		c.Writer.Flush()

		// 解析 SSE 数据
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				continue
			}
			// 尝试解析 usage
			var streamResp dto.ChatCompletionsStreamResponse
			if err := json.Unmarshal([]byte(data), &streamResp); err == nil {
				if streamResp.Usage != nil {
					usage = *streamResp.Usage
				}
			}
		}
	}
	return usage
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
