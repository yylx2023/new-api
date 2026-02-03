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
// 这将在 CLIProxyAPIPlus 的 openai-compatibility 配置中添加该 token 到 api-key-entries
// 使客户端可以使用 new-api 的 token 通过 CLIProxyAPIPlus 访问服务
func SyncTokenToCLIProxy(tokenKey string, tokenId int, userId int, tokenName string) error {
	fullKey := "sk-" + tokenKey

	// 1. 获取当前的 openai-compatibility 配置
	currentConfig, err := getOpenAICompatConfig()
	if err != nil {
		common.SysLog(fmt.Sprintf("CLIProxy sync: failed to get openai-compatibility config: %v", err))
		return err
	}

	// 2. 找到 CliProxySyncConfigName 配置项并添加新的 api-key-entry
	if err := addTokenToOpenAICompat(currentConfig, fullKey); err != nil {
		common.SysLog(fmt.Sprintf("CLIProxy sync: failed to add token to openai-compatibility: %v", err))
		return err
	}

	common.SysLog(fmt.Sprintf("CLIProxy sync: successfully synced token %d (user %d) to CLIProxyAPIPlus", tokenId, userId))
	return nil
}

// getOpenAICompatConfig 获取当前的 openai-compatibility 配置
func getOpenAICompatConfig() ([]map[string]interface{}, error) {
	url := CLIProxyAPIManagementURL + CLIProxyAPIOpenAICompatEndpoint

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if managementKey := getCLIProxyManagementKey(); managementKey != "" {
		req.Header.Set("Authorization", "Bearer "+managementKey)
	}

	resp, err := getCliProxySyncClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result struct {
		OpenAICompatibility []map[string]interface{} `json:"openai-compatibility"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.OpenAICompatibility, nil
}

// addTokenToOpenAICompat 将 token 添加到 openai-compatibility 配置的 api-key-entries 中
func addTokenToOpenAICompat(currentConfig []map[string]interface{}, tokenKey string) error {
	// 查找目标配置项的索引
	targetIndex := -1
	for i, cfg := range currentConfig {
		if name, ok := cfg["name"].(string); ok && name == CliProxySyncConfigName {
			targetIndex = i
			break
		}
	}

	if targetIndex == -1 {
		return fmt.Errorf("openai-compatibility config '%s' not found", CliProxySyncConfigName)
	}

	// 获取当前的 api-key-entries
	var currentEntries []map[string]interface{}
	if entries, ok := currentConfig[targetIndex]["api-key-entries"].([]interface{}); ok {
		for _, e := range entries {
			if entry, ok := e.(map[string]interface{}); ok {
				currentEntries = append(currentEntries, entry)
			}
		}
	}

	// 检查 token 是否已存在
	for _, entry := range currentEntries {
		if apiKey, ok := entry["api-key"].(string); ok && apiKey == tokenKey {
			// 已存在，无需添加
			return nil
		}
	}

	// 添加新的 api-key-entry
	currentEntries = append(currentEntries, map[string]interface{}{
		"api-key": tokenKey,
	})

	// 使用 PATCH 更新配置
	url := CLIProxyAPIManagementURL + CLIProxyAPIOpenAICompatEndpoint

	payload := map[string]interface{}{
		"name": CliProxySyncConfigName,
		"value": map[string]interface{}{
			"api-key-entries": currentEntries,
		},
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

	// 1. 获取当前的 openai-compatibility 配置
	currentConfig, err := getOpenAICompatConfig()
	if err != nil {
		common.SysLog(fmt.Sprintf("CLIProxy sync: failed to get openai-compatibility config: %v", err))
		return err
	}

	// 2. 从 api-key-entries 中删除该 token
	if err := removeTokenFromOpenAICompat(currentConfig, fullKey); err != nil {
		common.SysLog(fmt.Sprintf("CLIProxy sync: failed to remove token from openai-compatibility: %v", err))
		return err
	}

	common.SysLog(fmt.Sprintf("CLIProxy sync: successfully removed token from CLIProxyAPIPlus"))
	return nil
}

// removeTokenFromOpenAICompat 从 openai-compatibility 配置的 api-key-entries 中删除 token
func removeTokenFromOpenAICompat(currentConfig []map[string]interface{}, tokenKey string) error {
	// 查找目标配置项的索引
	targetIndex := -1
	for i, cfg := range currentConfig {
		if name, ok := cfg["name"].(string); ok && name == CliProxySyncConfigName {
			targetIndex = i
			break
		}
	}

	if targetIndex == -1 {
		// 配置不存在，视为成功
		return nil
	}

	// 获取当前的 api-key-entries
	var currentEntries []map[string]interface{}
	if entries, ok := currentConfig[targetIndex]["api-key-entries"].([]interface{}); ok {
		for _, e := range entries {
			if entry, ok := e.(map[string]interface{}); ok {
				currentEntries = append(currentEntries, entry)
			}
		}
	}

	// 过滤掉要删除的 token
	var newEntries []map[string]interface{}
	for _, entry := range currentEntries {
		if apiKey, ok := entry["api-key"].(string); ok && apiKey != tokenKey {
			newEntries = append(newEntries, entry)
		}
	}

	// 使用 PATCH 更新配置
	url := CLIProxyAPIManagementURL + CLIProxyAPIOpenAICompatEndpoint

	payload := map[string]interface{}{
		"name": CliProxySyncConfigName,
		"value": map[string]interface{}{
			"api-key-entries": newEntries,
		},
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
