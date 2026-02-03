package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

const (
	// CLIProxyAPIManagementURL 是 CLIProxyAPIPlus 管理平台的默认地址
	CLIProxyAPIManagementURL = "http://127.0.0.1:8319"

	// CLIProxyAPIOpenAICompatEndpoint 是 openai-compatibility 管理 API 端点
	CLIProxyAPIOpenAICompatEndpoint = "/v0/management/openai-compatibility"

	// CLIProxyAPIAPIKeysEndpoint 是 api-keys 管理 API 端点
	CLIProxyAPIAPIKeysEndpoint = "/v0/management/api-keys"

	// CliProxySyncConfigName 是在 CLIProxyAPIPlus 中注册的 openai-compatibility 配置名称
	CliProxySyncConfigName = "new-api-billing"
)

var (
	cliProxySyncClient *http.Client
	cliProxySyncOnce   sync.Once
)

// getCliProxySyncClient 返回用于同步的 HTTP 客户端
func getCliProxySyncClient() *http.Client {
	cliProxySyncOnce.Do(func() {
		cliProxySyncClient = &http.Client{
			Timeout: 10 * time.Second,
		}
	})
	return cliProxySyncClient
}

// getCLIProxyManagementKey 获取 CLIProxyAPIPlus 管理密钥
// 优先从环境变量 CLIPROXY_MANAGEMENT_KEY 读取
// 如果未设置，返回空字符串（localhost 访问不需要认证）
func getCLIProxyManagementKey() string {
	key := os.Getenv("CLIPROXY_MANAGEMENT_KEY")
	return strings.TrimSpace(key)
}

// OpenAICompatibilityAPIKey 表示 CLIProxyAPIPlus 的 API Key 配置
type OpenAICompatibilityAPIKey struct {
	APIKey   string `json:"api-key"`
	ProxyURL string `json:"proxy-url,omitempty"`
}

// OpenAICompatibilityModel 表示 CLIProxyAPIPlus 的模型配置
type OpenAICompatibilityModel struct {
	Name  string `json:"name"`
	Alias string `json:"alias"`
}

// OpenAICompatibilityEntry 表示 CLIProxyAPIPlus 的 openai-compatibility 配置项
type OpenAICompatibilityEntry struct {
	Name          string                      `json:"name"`
	BaseURL       string                      `json:"base-url"`
	APIKeyEntries []OpenAICompatibilityAPIKey `json:"api-key-entries,omitempty"`
	Models        []OpenAICompatibilityModel  `json:"models,omitempty"`
	Headers       map[string]string           `json:"headers,omitempty"`
}

// SyncTokenToCLIProxy 将 new-api 的 token 同步到 CLIProxyAPIPlus
// 这将在 CLIProxyAPIPlus 的 api-keys 列表中添加该 token
// 使客户端可以使用 new-api 的 token 通过 CLIProxyAPIPlus 访问服务
func SyncTokenToCLIProxy(tokenKey string, tokenId int, userId int, tokenName string) error {
	fullKey := "sk-" + tokenKey

	// 1. 将 token 添加到 CLIProxyAPIPlus 的 api-keys 列表
	if err := addTokenToAPIKeys(fullKey); err != nil {
		common.SysLog(fmt.Sprintf("CLIProxy sync: failed to add token to api-keys: %v", err))
		return err
	}

	common.SysLog(fmt.Sprintf("CLIProxy sync: successfully synced token %d (user %d) to CLIProxyAPIPlus", tokenId, userId))
	return nil
}

// addTokenToAPIKeys 将 token 添加到 CLIProxyAPIPlus 的 api-keys 列表
func addTokenToAPIKeys(tokenKey string) error {
	url := CLIProxyAPIManagementURL + CLIProxyAPIAPIKeysEndpoint

	// 使用 PATCH 方法添加新的 api-key
	payload := map[string]interface{}{
		"add": []string{tokenKey},
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("PATCH", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 添加管理密钥认证（如果配置了）
	if managementKey := getCLIProxyManagementKey(); managementKey != "" {
		req.Header.Set("Authorization", "Bearer "+managementKey)
	}

	resp, err := getCliProxySyncClient().Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}

// DeleteTokenFromCLIProxy 从 CLIProxyAPIPlus 删除 token
func DeleteTokenFromCLIProxy(tokenKey string) error {
	fullKey := "sk-" + tokenKey

	// 从 api-keys 列表中删除
	if err := removeTokenFromAPIKeys(fullKey); err != nil {
		common.SysLog(fmt.Sprintf("CLIProxy sync: failed to remove token from api-keys: %v", err))
		return err
	}

	common.SysLog(fmt.Sprintf("CLIProxy sync: successfully removed token from CLIProxyAPIPlus"))
	return nil
}

// removeTokenFromAPIKeys 从 CLIProxyAPIPlus 的 api-keys 列表中删除 token
func removeTokenFromAPIKeys(tokenKey string) error {
	url := CLIProxyAPIManagementURL + CLIProxyAPIAPIKeysEndpoint +
		"?key=" + strings.TrimSpace(tokenKey)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// 添加管理密钥认证（如果配置了）
	if managementKey := getCLIProxyManagementKey(); managementKey != "" {
		req.Header.Set("Authorization", "Bearer "+managementKey)
	}

	resp, err := getCliProxySyncClient().Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// DELETE 可能返回 200 或 404（如果 key 不存在），都认为成功
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}
