package controller

import (
	"bytes"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

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

// WukongChatStream 转发请求到后端服务
// POST /usage/api/chat-stream
func WukongChatStream(c *gin.Context) {
	tokenId := c.GetInt("token_id")

	// 1. 获取 token 验证余额
	token, err := model.GetTokenById(tokenId)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error":   "令牌无效",
		})
		return
	}

	// 2. 检查余额
	if !token.UnlimitedQuota && token.RemainQuota <= 0 {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"error":   "余额不足",
		})
		return
	}

	// 3. 转发请求到后端 (Augment-BYOK-Proxy)
	// 可通过环境变量配置: WUKONG_PROXY_URL (完整URL) 或 WUKONG_PROXY_HOST + WUKONG_PROXY_PORT
	targetURL := common.GetEnvOrDefaultString("WUKONG_PROXY_URL", "")
	if targetURL == "" {
		host := common.GetEnvOrDefaultString("WUKONG_PROXY_HOST", "127.0.0.1")
		port := common.GetEnvOrDefaultString("WUKONG_PROXY_PORT", "8318")
		targetURL = "http://" + host + ":" + port + "/chat-stream"
	}

	// 读取请求体
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "读取请求体失败",
		})
		return
	}

	// 创建转发请求
	req, err := http.NewRequest("POST", targetURL, bytes.NewReader(body))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   "创建请求失败",
		})
		return
	}

	// 复制请求头
	for key, values := range c.Request.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// 设置 Rust 端 (Augment-BYOK-Proxy) 需要的鉴权 token
	// 对应 config.yaml 中的 proxy.auth_token
	proxyAuthToken := common.GetEnvOrDefaultString("PROXY_AUTH_TOKEN", "wukong")
	req.Header.Set("X-Api-Key", proxyAuthToken)

	// 发送请求
	client := service.GetHttpClient()
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"success": false,
			"error":   "后端服务请求失败: " + err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	// 流式响应
	c.Status(resp.StatusCode)
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}

	// 流式传输响应
	io.Copy(c.Writer, resp.Body)
}

