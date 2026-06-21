package internal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

// generateRandomIP 生成随机 IP 地址用于 X-Forwarded-For
func generateRandomIP() string {
	// 生成看起来合理的公网 IP
	// 避免保留地址段：10.x, 172.16-31.x, 192.168.x, 127.x
	firstOctet := []int{36, 42, 58, 60, 61, 101, 106, 110, 111, 112, 113, 114, 115, 116, 117, 118, 119, 120, 121, 122, 123, 124, 125, 139, 140, 144, 150, 153, 157, 163, 171, 175, 180, 182, 183, 202, 210, 211, 218, 219, 220, 221, 222, 223}
	first := firstOctet[rand.Intn(len(firstOctet))]
	return fmt.Sprintf("%d.%d.%d.%d", first, rand.Intn(256), rand.Intn(256), rand.Intn(254)+1)
}

// APIError OpenAI 兼容的错误格式
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// ErrorResponse 错误响应结构
type ErrorResponse struct {
	Error APIError `json:"error"`
}

// 错误类型常量
const (
	ErrTypeInvalidRequest = "invalid_request_error"
	ErrTypeAuthentication = "authentication_error"
	ErrTypeNotFound       = "not_found_error"
	ErrTypeServer         = "server_error"
	ErrTypeUpstream       = "upstream_error"
)

// writeError 写入错误响应
func writeError(w http.ResponseWriter, statusCode int, errType, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(ErrorResponse{
		Error: APIError{
			Message: message,
			Type:    errType,
			Code:    code,
		},
	})
}

// writeErrorResponse 统一错误响应（通用请求失败）
func writeErrorResponse(w http.ResponseWriter, statusCode int) {
	writeError(w, statusCode, ErrTypeServer, "请求失败", "")
}

// writeInvalidRequestError 无效请求错误
func writeInvalidRequestError(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadRequest, ErrTypeInvalidRequest, message, "invalid_request")
}

// writeModelNotFoundError 模型不存在错误
func writeModelNotFoundError(w http.ResponseWriter, model string) {
	writeError(w, http.StatusNotFound, ErrTypeNotFound,
		fmt.Sprintf("模型 '%s' 不存在", model), "model_not_found")
}

// writeUpstreamError 上游错误（透传）
func writeUpstreamError(w http.ResponseWriter, statusCode int, upstreamBody []byte) {
	// 尝试解析上游错误
	var upstreamErr struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}

	message := "请求失败"
	if err := json.Unmarshal(upstreamBody, &upstreamErr); err == nil {
		if upstreamErr.Error.Message != "" {
			message = upstreamErr.Error.Message
		} else if upstreamErr.Message != "" {
			message = upstreamErr.Message
		} else if upstreamErr.Msg != "" {
			message = upstreamErr.Msg
		}
	}

	writeError(w, statusCode, ErrTypeUpstream, message, "upstream_error")
}

func extractLatestUserContent(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			text, _ := messages[i].ParseContent()
			return text
		}
	}
	return ""
}

// mergeSystemIntoFirstUser 把所有 role:system 消息提取出来，拼到第一条 user
// 消息前面。z.ai 会覆盖客户端的 system 消息，导致 system prompt 丢失；但 user
// 消息会被保留，所以把 system 内容塞进 user 消息开头能生效。
// 多条 system 用换行拼接；保留原文，不做格式化。没有 system 或没有 user 时原样返回。
func mergeSystemIntoFirstUser(messages []Message) []Message {
	// 收集所有 system 消息内容
	var sysParts []string
	firstUserIdx := -1
	out := make([]Message, 0, len(messages))
	for i, msg := range messages {
		if msg.Role == "system" {
			text, _ := msg.ParseContent()
			if strings.TrimSpace(text) != "" {
				sysParts = append(sysParts, text)
			}
			continue // 跳过 system，不放进 out
		}
		if firstUserIdx < 0 && msg.Role == "user" {
			firstUserIdx = len(out) // 记录 out 里的位置（system 已被剔除）
		}
		out = append(out, msg)
		_ = i
	}
	if len(sysParts) == 0 || firstUserIdx < 0 {
		// 没有 system 或没有 user，原样返回（但 out 可能已剔除 system——
		// 没有 user 时 system 无处可塞，也只能丢弃）
		if len(sysParts) == 0 {
			return messages
		}
		return out
	}
	// 把 system 内容拼到第一条 user 消息前面。
	// z.ai 会覆盖客户端的 system 角色，所以把 system 内容用 XML 标签包裹后
	// 塞进第一条 user 消息（user 消息 z.ai 会保留）。GLM 对 XML 标签理解好，
	// <system_prompt> 标签能让模型区分这是系统指令而非用户输入。
	// 多模态消息（content 是 []interface{}）只改文本部分，保留图片/视频部分。
	sysText := strings.Join(sysParts, "\n\n")
	prefixed := "<system_prompt>\n" + sysText + "\n</system_prompt>\n\n"
	u := &out[firstUserIdx]
	switch c := u.Content.(type) {
	case string:
		u.Content = prefixed + c
	case []interface{}:
		// 多模态：找到第一个 text part 前面插，没有 text part 就加一个
		newContent := make([]interface{}, 0, len(c)+1)
		newContent = append(newContent, map[string]interface{}{"type": "text", "text": prefixed})
		newContent = append(newContent, c...)
		u.Content = newContent
	case nil:
		u.Content = prefixed
	default:
		// 其它类型：转字符串拼接
		u.Content = prefixed + fmt.Sprintf("%v", c)
	}
	return out
}

func extractAllMediaURLs(messages []Message) (imageURLs, videoURLs []string) {
	for _, msg := range messages {
		_, imgs, vids := msg.ParseContentFull()
		imageURLs = append(imageURLs, imgs...)
		videoURLs = append(videoURLs, vids...)
	}
	return imageURLs, videoURLs
}

func buildBrowserFingerprintQuery(timestamp int64, requestID, userID, token, chatID string) string {
	currentURL := fmt.Sprintf("https://chat.z.ai/c/%s", chatID)
	pathname := fmt.Sprintf("/c/%s", chatID)
	localTime := time.UnixMilli(timestamp).UTC().Format("2006-01-02T15:04:05.000Z")
	utcTime := time.UnixMilli(timestamp).UTC().Format(time.RFC1123)

	q := url.Values{}
	q.Set("timestamp", strconv.FormatInt(timestamp, 10))
	q.Set("requestId", requestID)
	q.Set("user_id", userID)
	q.Set("version", "0.0.1")
	q.Set("platform", "web")
	q.Set("token", token)
	q.Set("user_agent", BrowserUserAgent)
	q.Set("language", "en-US")
	q.Set("languages", "en-US,en")
	q.Set("timezone", "Asia/Shanghai")
	q.Set("cookie_enabled", "true")
	q.Set("screen_width", "1920")
	q.Set("screen_height", "1080")
	q.Set("screen_resolution", "1920x1080")
	q.Set("viewport_height", "929")
	q.Set("viewport_width", "1920")
	q.Set("viewport_size", "1920x929")
	q.Set("color_depth", "24")
	q.Set("pixel_ratio", "1")
	q.Set("current_url", currentURL)
	q.Set("pathname", pathname)
	q.Set("search", "")
	q.Set("hash", "")
	q.Set("host", "chat.z.ai")
	q.Set("hostname", "chat.z.ai")
	q.Set("protocol", "https:")
	q.Set("referrer", "")
	q.Set("title", "Z.ai - Free AI Chatbot & Agent powered by GLM-5.1 & GLM-5")
	q.Set("timezone_offset", "-480")
	q.Set("local_time", localTime)
	q.Set("utc_time", utcTime)
	q.Set("is_mobile", "false")
	q.Set("is_touch", "false")
	q.Set("max_touch_points", "0")
	q.Set("browser_name", "Chrome")
	q.Set("os_name", "Windows")
	q.Set("signature_timestamp", strconv.FormatInt(timestamp, 10))

	return q.Encode()
}

type bridgeUpstreamResponse struct {
	OK         bool              `json:"ok"`
	Status     int               `json:"status"`
	StatusText string            `json:"statusText"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	Error      string            `json:"error"`
}

func makeBridgeRequest(upstreamURL, token, signature string, bodyBytes []byte, randomIP string) (*fhttp.Response, error) {
	bridgeURL := strings.TrimRight(Cfg.BrowserBridgeURL, "/") + "/v1/upstream"
	payload := map[string]interface{}{
		"method": "POST",
		"url":    upstreamURL,
		"headers": map[string]string{
			"Authorization":   "Bearer " + token,
			"X-FE-Version":    GetFeVersion(),
			"X-Signature":     signature,
			"Content-Type":    "application/json",
			"X-Forwarded-For": randomIP,
			"X-Real-IP":       randomIP,
		},
		"body": string(bodyBytes),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := fhttp.NewRequest("POST", bridgeURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if Cfg.BrowserBridgeSecret != "" {
		req.Header.Set("X-Bridge-Secret", Cfg.BrowserBridgeSecret)
	}

	client, err := TLSHTTPClient(300 * time.Second)
	if err != nil {
		return nil, err
	}
	bridgeResp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer bridgeResp.Body.Close()

	bridgeBody, err := io.ReadAll(bridgeResp.Body)
	if err != nil {
		return nil, err
	}
	if bridgeResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("browser bridge status %d: %s", bridgeResp.StatusCode, string(bridgeBody)[:min(500, len(bridgeBody))])
	}

	var decoded bridgeUpstreamResponse
	if err := json.Unmarshal(bridgeBody, &decoded); err != nil {
		return nil, fmt.Errorf("decode browser bridge response: %w", err)
	}
	if !decoded.OK {
		if decoded.Error == "" {
			decoded.Error = "unknown browser bridge error"
		}
		return nil, fmt.Errorf("browser bridge error: %s", decoded.Error)
	}
	if decoded.Status == 0 {
		return nil, fmt.Errorf("browser bridge returned empty upstream status")
	}

	resp := &fhttp.Response{
		StatusCode: decoded.Status,
		Status:     fmt.Sprintf("%d %s", decoded.Status, decoded.StatusText),
		Header:     make(fhttp.Header),
		Body:       io.NopCloser(strings.NewReader(decoded.Body)),
	}
	for k, v := range decoded.Headers {
		resp.Header.Set(k, v)
	}
	return resp, nil
}

// makeFullProxyRequest 把整个 chat 请求转给「JSDOM 全链路 chat 代理」provider。
// provider 在同一个 JSDOM window 内完成：拿 captcha → 建 chats/new 会话 → 发 completions，
// 并把上游 SSE 原文透传回来。Go 侧零改动复用现有 SSE 解析（handleStream/NonStream）。
//
// 入参：已映射的上游 model id + thinking/search 标志 + signature_prompt + upstreamMessages（含 fileID）+ filesData。
// 返回：包成 *fhttp.Response 的 SSE 流（StatusCode 上游原值，通常 200）。
func makeFullProxyRequest(token, upstreamModel string, enableThinking, autoWebSearch bool, reasoningEffort, signaturePrompt string, upstreamMessages []map[string]interface{}, filesData []map[string]interface{}, contextFileUploaded bool) (*fhttp.Response, error) {
	proxyURL := strings.TrimRight(Cfg.CaptchaFullProxyURL, "/") + "/v1/chat"

	payload := map[string]interface{}{
		"token":            token,
		"upstream_model":   upstreamModel,
		"enable_thinking":  enableThinking,
		"auto_web_search":  autoWebSearch,
		"signature_prompt": signaturePrompt,
		"messages":         upstreamMessages,
		// context_file_uploaded：Go 侧已把对话历史上传成文件附到 files 数组。
		// provider（chat_proxy.cjs）据此跳过自己的「合并到最后一条消息」逻辑，避免重复注入上下文。
		"context_file_uploaded": contextFileUploaded,
	}
	if reasoningEffort != "" {
		payload["reasoning_effort"] = reasoningEffort
	}
	if len(filesData) > 0 {
		payload["files"] = filesData
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", proxyURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// 关键：禁用 gzip。Go http.Client 默认发 Accept-Encoding: gzip，
	// Node/上游若返回 gzip 响应，Go transport 会缓冲整个 body 解压 → 伪流式。
	req.Header.Set("Accept-Encoding", "identity")

	// provider 是 localhost 内部通信，不用 TLSHTTPClient（tls-client 会缓冲流式数据，
	// 导致 SSE 攒批成"伪流式"）。改用标准 http.Client，实时读流。
	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	// provider 出错（非 2xx）：读出来报错；成功：透传 body 给 SSE 解析。
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		preview := string(body)
		if len(preview) > 500 {
			preview = preview[:500]
		}
		return nil, fmt.Errorf("full-proxy /v1/chat status %d: %s", resp.StatusCode, preview)
	}

	// 透传：provider 的 body 就是上游 SSE 原文，直接交给下游解析。
	// 用 fhttp.Response 包一层（下游 handleStream 用标准 io.Reader 读），Content-Type 设成 text/event-stream。
	out := &fhttp.Response{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Header:     fhttp.Header{},
		Body:       resp.Body,
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/event-stream; charset=utf-8"
	}
	out.Header.Set("Content-Type", ct)
	return out, nil
}

// makeHybridRequest 混合模式：provider（JSDOM）只拿 captcha+chatId，
// Go tls-client（Chrome 指纹）发 chat。适用于 Node 直连被 CDN 风控（405）的环境。
func makeHybridRequest(token, upstreamModel string, enableThinking, autoWebSearch bool, reasoningEffort, signaturePrompt string, upstreamMessages []map[string]interface{}, filesData []map[string]interface{}) (*fhttp.Response, error) {
	// 1) 从 provider 拿 captcha + chatId
	proxyURL := strings.TrimRight(Cfg.CaptchaFullProxyURL, "/") + "/v1/captcha-token"
	reqPayload := map[string]interface{}{"token": token}
	reqBytes, _ := json.Marshal(reqPayload)

	hreq, err := http.NewRequest("POST", proxyURL, bytes.NewReader(reqBytes))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept-Encoding", "identity")
	hclient := &http.Client{Timeout: 60 * time.Second}
	hresp, err := hclient.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("hybrid: get captcha from provider: %w", err)
	}
	defer hresp.Body.Close()
	if hresp.StatusCode != 200 {
		body, _ := io.ReadAll(hresp.Body)
		return nil, fmt.Errorf("hybrid: provider captcha-token status %d: %s", hresp.StatusCode, string(body)[:min(200, len(body))])
	}
	var capResp struct {
		OK                  bool   `json:"ok"`
		CaptchaVerifyParam  string `json:"captcha_verify_param"`
		ChatID              string `json:"chat_id"`
		UserMsgID           string `json:"user_msg_id"`
		Error               string `json:"error"`
	}
	if err := json.NewDecoder(hresp.Body).Decode(&capResp); err != nil {
		return nil, fmt.Errorf("hybrid: decode captcha response: %w", err)
	}
	if !capResp.OK || capResp.CaptchaVerifyParam == "" {
		return nil, fmt.Errorf("hybrid: no captcha: %s", capResp.Error)
	}
	LogDebug("[Hybrid] Got captcha + chatId=%s from provider", capResp.ChatID)

	// 2) 用 Go tls-client（Chrome 指纹）发 completions
	payload, err := DecodeJWTPayload(token)
	if err != nil || payload == nil {
		return nil, fmt.Errorf("invalid token")
	}
	userID := payload.ID
	chatID := capResp.ChatID
	if chatID == "" {
		chatID = uuid.New().String()
	}
	userMsgID := capResp.UserMsgID
	if userMsgID == "" {
		userMsgID = uuid.New().String()
	}
	ts := time.Now().UnixMilli()
	requestID := uuid.New().String()
	sig := GenerateSignature(userID, requestID, signaturePrompt, ts)
	effort := ""
	if enableThinking && !autoWebSearch {
		effort = mapEffort(reasoningEffort)
	}

	completionsURL := Cfg.APIEndpoint + "?" + buildBrowserFingerprintQuery(ts, requestID, userID, token, chatID)

	// 构造 variables
	now := time.Now()
	sh := now.UTC().Add(8 * 3600 * time.Second)
	weekdays := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	curDT := fmt.Sprintf("%d-%02d-%02d %02d:%02d:%02d", sh.Year(), sh.Month(), sh.Day(), sh.Hour(), sh.Minute(), sh.Second())

	body := map[string]interface{}{
		"stream":           true,
		"model":            upstreamModel,
		"messages":         upstreamMessages,
		"signature_prompt": signaturePrompt,
		"params":           map[string]interface{}{},
		"extra":            map[string]interface{}{},
		"features": map[string]interface{}{
			"image_generation":    false,
			"web_search":          false,
			"auto_web_search":     autoWebSearch,
			"preview_mode":        enableThinking,
			"flags":               []string{},
			"vlm_tools_enable":    false,
			"vlm_web_search_enable": false,
			"vlm_website_mode":    false,
			"enable_thinking":     enableThinking,
		},
		"variables": map[string]string{
			"{{USER_NAME}}": "Anonymous", "{{USER_LOCATION}}": "Unknown",
			"{{CURRENT_DATETIME}}": curDT, "{{CURRENT_DATE}}": curDT[:10],
			"{{CURRENT_TIME}}": curDT[11:], "{{CURRENT_WEEKDAY}}": weekdays[sh.Weekday()],
			"{{CURRENT_TIMEZONE}}": "Asia/Shanghai", "{{USER_LANGUAGE}}": "zh-CN",
		},
		"chat_id":                     chatID,
		"id":                          uuid.New().String(),
		"current_user_message_id":     userMsgID,
		"current_user_message_parent_id": nil,
		"background_tasks":            map[string]bool{"title_generation": true, "tags_generation": true},
		"captcha_verify_param":        capResp.CaptchaVerifyParam,
	}
	if effort != "" {
		body["features"].(map[string]interface{})["reasoning_effort"] = effort
	}
	if len(filesData) > 0 {
		body["files"] = filesData
	}
	bodyBytes, _ := json.Marshal(body)

	freq, err := fhttp.NewRequest("POST", completionsURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	randomIP := generateRandomIP()
	freq.Header.Set("Authorization", "Bearer "+token)
	freq.Header.Set("X-FE-Version", GetFeVersion())
	freq.Header.Set("X-Signature", sig)
	freq.Header.Set("Content-Type", "application/json")
	freq.Header.Set("Accept-Language", "zh-CN")
	freq.Header.Set("X-Region", "overseas")
	freq.Header.Set("Connection", "keep-alive")
	freq.Header.Set("Origin", "https://chat.z.ai")
	freq.Header.Set("Referer", fmt.Sprintf("https://chat.z.ai/c/%s", chatID))
	ApplyBrowserFingerprintHeaders(freq.Header)
	freq.Header.Set("X-Forwarded-For", randomIP)
	freq.Header.Set("X-Real-IP", randomIP)

	LogDebug("[Hybrid] Sending completions via tls-client, model=%s, XFF=%s", upstreamModel, randomIP)
	client, err := TLSHTTPClient(300 * time.Second)
	if err != nil {
		return nil, err
	}
	return client.Do(freq)
}

// mapEffort 映射 reasoning_effort（与 chat_proxy.cjs 的 mapEffort 一致）。
func mapEffort(v string) string {
	if v == "" {
		return ""
	}
	lv := strings.ToLower(v)
	if lv == "max" || lv == "high" {
		return "max"
	}
	return "high"
}
func makeUpstreamRequest(token string, messages []Message, model string, imageURLs, videoURLs []string, hasTools bool, tools []Tool, reasoningEffort string) (*fhttp.Response, string, error) {
	payload, err := DecodeJWTPayload(token)
	if err != nil || payload == nil {
		return nil, "", fmt.Errorf("invalid token")
	}

	userID := payload.ID
	chatID := uuid.New().String()
	timestamp := time.Now().UnixMilli()
	requestID := uuid.New().String()
	userMsgID := uuid.New().String()

	// 使用新的模型映射系统
	mapping := GetUpstreamConfig(model)
	var targetModel string
	var enableThinking, autoWebSearch bool
	var mcpServers []string

	if mapping != nil {
		targetModel = mapping.UpstreamModelID
		enableThinking = mapping.EnableThinking
		autoWebSearch = mapping.AutoWebSearch
		mcpServers = mapping.MCPServers
		LogDebug("Model mapping: %s -> %s (thinking=%v, search=%v)", model, targetModel, enableThinking, autoWebSearch)
	} else {
		// 回退到老的逻辑
		targetModel = GetTargetModel(model)
		enableThinking = IsThinkingModel(model)
		autoWebSearch = IsSearchModel(model)
		LogDebug("Using fallback model mapping: %s -> %s", model, targetModel)
	}

	if targetModel == "glm-4.5v" || targetModel == "glm-4.6v" {
		autoWebSearch = false
	}

	if hasTools {
		autoWebSearch = false
		LogDebug("[Upstream] Disabled auto web search because custom tools were provided")
	}
	if len(imageURLs) > 0 || len(videoURLs) > 0 {
		vlmServers := []string{"vlm-image-search", "vlm-image-recognition", "vlm-image-processing"}
		existingSet := make(map[string]bool)
		for _, s := range mcpServers {
			existingSet[s] = true
		}
		for _, s := range vlmServers {
			if !existingSet[s] {
				mcpServers = append(mcpServers, s)
			}
		}
	}

	latestUserContent := extractLatestUserContent(messages)

	signature := GenerateSignature(userID, requestID, latestUserContent, timestamp)

	url := Cfg.APIEndpoint + "?" + buildBrowserFingerprintQuery(timestamp, requestID, userID, token, chatID)

	urlToFileID := make(map[string]string)
	var filesData []map[string]interface{}

	// 上传图片
	if len(imageURLs) > 0 {
		LogDebug("[Upstream] Uploading %d images...", len(imageURLs))
		imageFiles, _ := UploadImages(token, imageURLs)
		LogDebug("[Upstream] Image upload result: %d files", len(imageFiles))
		for i, f := range imageFiles {
			if i < len(imageURLs) {
				urlToFileID[imageURLs[i]] = f.ID
			}
			filesData = append(filesData, map[string]interface{}{
				"type":            f.Type,
				"file":            f.File,
				"id":              f.ID,
				"url":             f.URL,
				"name":            f.Name,
				"status":          f.Status,
				"size":            f.Size,
				"error":           f.Error,
				"itemId":          f.ItemID,
				"media":           f.Media,
				"ref_user_msg_id": userMsgID,
			})
		}
	}

	// 上传视频
	if len(videoURLs) > 0 {
		LogDebug("[Upstream] Uploading %d videos...", len(videoURLs))
		videoFiles, _ := UploadVideos(token, videoURLs)
		LogDebug("[Upstream] Video upload result: %d files", len(videoFiles))
		for i, f := range videoFiles {
			if i < len(videoURLs) {
				urlToFileID[videoURLs[i]] = f.ID
			}
			filesData = append(filesData, map[string]interface{}{
				"type":            f.Type,
				"file":            f.File,
				"id":              f.ID,
				"url":             f.URL,
				"name":            f.Name,
				"status":          f.Status,
				"size":            f.Size,
				"error":           f.Error,
				"itemId":          f.ItemID,
				"media":           f.Media,
				"ref_user_msg_id": userMsgID,
			})
		}
	}
	var upstreamMessages []map[string]interface{}
	for _, msg := range messages {
		upstreamMessages = append(upstreamMessages, msg.ToUpstreamMessage(urlToFileID))
	}

	// ── 多轮上下文：优先把对话历史上传成 .txt 文件（z.ai 文件接口）附到 files 数组。
	// z.ai 后端不读 messages 数组里的历史（只看 chat_id 服务端历史，而每次新建 chat_id），
	// 所以历史要么上传成文件，要么合并到最后一条 user message。
	// 这里只做「上传文件」优先策略；上传失败时 contextFileUploaded=false，
	// 全代理路径下由 chat_proxy.cjs 的合并逻辑兜底（fallback 保留）。
	// 思路参考 CJackHwang/ds2api 的 current_input_file（DS2API_HISTORY.txt）。
	contextFileUploaded := false
	if Cfg.ContextFileUpload && len(messages) > 1 {
		lastUserIdx := -1
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" {
				lastUserIdx = i
				break
			}
		}
		if lastUserIdx > 0 {
			transcript := buildHistoryTranscript(messages, lastUserIdx)
			if ctxFile, ferr := UploadContextFile(token, transcript); ferr == nil && ctxFile != nil {
				filesData = append(filesData, map[string]interface{}{
					"type":            ctxFile.Type,
					"file":            ctxFile.File,
					"id":              ctxFile.ID,
					"url":             ctxFile.URL,
					"name":            ctxFile.Name,
					"status":          ctxFile.Status,
					"size":            ctxFile.Size,
					"error":           ctxFile.Error,
					"itemId":          ctxFile.ItemID,
					"media":           ctxFile.Media,
					"uploadedAt":      ctxFile.UploadedAt,
					"ref_user_msg_id": userMsgID,
				})
				contextFileUploaded = true
				LogDebug("[ContextFile] Attached history file id=%s to request", ctxFile.ID)
			}
		}
	}

	// ── 全链路 chat 代理：整个请求转给 captcha-provider（同 JSDOM 环境拿 captcha+建会话+发 completions）──
	// 设了 CaptchaFullProxyURL 就走这条，彻底绕开跨进程环境不一致导致的 F019 verify_failed。
	// provider 返回上游 SSE 原文，下游 handleStream/NonStream 零改动复用。
	if Cfg.CaptchaFullProxyURL != "" {
		// HYBRID_MODE=1：JSDOM 只拿 captcha，Go tls-client（Chrome 指纹）发 chat。
		// 适用于 Node 直连被 CDN 风控（HF 数据中心 IP → 405）。
		if osGetenvBool("HYBRID_MODE") {
			LogDebug("[Hybrid] captcha from provider, chat via tls-client")
			resp, hErr := makeHybridRequest(token, targetModel, enableThinking, autoWebSearch, reasoningEffort, latestUserContent, upstreamMessages, filesData)
			if hErr != nil {
				return nil, targetModel, hErr
			}
			return resp, targetModel, nil
		}
		LogDebug("[FullProxy] routing %s via %s", targetModel, Cfg.CaptchaFullProxyURL)
		resp, fpErr := makeFullProxyRequest(token, targetModel, enableThinking, autoWebSearch, reasoningEffort, latestUserContent, upstreamMessages, filesData, contextFileUploaded)
		if fpErr != nil {
			return nil, targetModel, fpErr
		}
		return resp, targetModel, nil
	}

	// 普通 chat 模式（不使用 z.ai 内部 agent）。
	// 工具调用通过 prompt injection 实现，模型输出 XML 工具调用 → 解析为 OpenAI tool_calls。
	// z.ai agent 模式虽然能调用内置工具但不返回 tool_calls 字段，对外部客户端无用，已废弃。
	flags := []string{}
	previewMode := false
	imageGen := true
	webSearch := true

	body := map[string]interface{}{
		"stream":           true,
		"model":            targetModel,
		"messages":         upstreamMessages,
		"signature_prompt": latestUserContent,
		"params":           map[string]interface{}{},
		"extra":            map[string]interface{}{},
		"features": map[string]interface{}{
			"image_generation":      imageGen,
			"web_search":            webSearch,
			"auto_web_search":       autoWebSearch && !hasTools,
			"preview_mode":          previewMode,
			"flags":                 flags,
			"vlm_tools_enable":      false,
			"vlm_web_search_enable": false,
			"vlm_website_mode":      false,
			"enable_thinking":       enableThinking,
		},
		"chat_id": chatID,
		"id":      uuid.New().String(),
	}

	// (Agent 模式相关字段已移除 — z.ai agent 不返回 tool_calls，对外部客户端无用)

	// 原生 function calling：把 tools 传给上游，设置 params.function_calling = "native"
	// 注意：z.ai 的原生 function calling 格式可能和 OpenAI 不同，暂时禁用
	// if hasTools && len(tools) > 0 {
	// 	body["tools"] = tools
	// 	body["params"] = map[string]interface{}{
	// 		"function_calling": "native",
	// 	}
	// }

	if len(mcpServers) > 0 {
		body["mcp_servers"] = mcpServers
	}

	// 注入 captcha_verify_param（如果配置了 captcha provider）
	if Cfg.CaptchaProviderURL != "" {
		captchaToken, err := fetchCaptchaToken()
		if err != nil {
			LogWarn("[Captcha] Failed to get captcha token: %v", err)
		} else if captchaToken != "" {
			body["captcha_verify_param"] = captchaToken
			LogDebug("[Captcha] Injected captcha_verify_param")
		}
	}

	if len(filesData) > 0 {
		body["files"] = filesData
		body["current_user_message_id"] = userMsgID
		LogDebug("[Upstream] Attaching %d files to request, userMsgID=%s", len(filesData), userMsgID)
		for i, fd := range filesData {
			LogDebug("[Upstream] File %d: id=%v, type=%v, name=%v, status=%v", i+1, fd["id"], fd["type"], fd["name"], fd["status"])
		}
	}

	bodyBytes, _ := json.Marshal(body)

	req, err := fhttp.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, "", err
	}

	randomIP := generateRandomIP()

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-FE-Version", GetFeVersion())
	req.Header.Set("X-Signature", signature)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Origin", "https://chat.z.ai")
	req.Header.Set("Referer", fmt.Sprintf("https://chat.z.ai/c/%s", chatID))
	ApplyBrowserFingerprintHeaders(req.Header)
	req.Header.Set("X-Forwarded-For", randomIP)
	req.Header.Set("X-Real-IP", randomIP)

	LogDebug("Upstream request: model=%s, messages=%d, XFF=%s", targetModel, len(messages), randomIP)

	var resp *fhttp.Response
	if Cfg.BrowserBridgeURL != "" {
		LogDebug("[BrowserBridge] Forwarding upstream request via %s", Cfg.BrowserBridgeURL)
		resp, err = makeBridgeRequest(url, token, signature, bodyBytes, randomIP)
	} else {
		client, clientErr := TLSHTTPClient(300 * time.Second)
		if clientErr != nil {
			return nil, "", clientErr
		}
		resp, err = client.Do(req)
	}
	if err != nil {
		return nil, "", err
	}

	LogDebug("Upstream response: status=%d, XFF=%s", resp.StatusCode, randomIP)
	return resp, targetModel, nil
}

type UpstreamData struct {
	Type string `json:"type"`
	Data struct {
		DeltaContent string `json:"delta_content"`
		EditContent  string `json:"edit_content"`
		Phase        string `json:"phase"`
		Done         bool   `json:"done"`
		Error        *struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"error,omitempty"`
	} `json:"data"`
}

// HasError 检查上游响应是否包含错误
func (u *UpstreamData) HasError() bool {
	return u.Data.Error != nil && u.Data.Error.Code != ""
}

// GetErrorMessage 获取错误信息
func (u *UpstreamData) GetErrorMessage() string {
	if u.Data.Error == nil {
		return ""
	}
	if u.Data.Error.Detail != "" {
		return u.Data.Error.Detail
	}
	return u.Data.Error.Code
}

// UpstreamResult 上游请求结果
type UpstreamResult struct {
	Success         bool
	HasContent      bool
	ResponseStarted bool
	ErrorMessage    string
	OutputTokens    int64
}

const RetryableErr = "INTERNAL_ERROR"

// fullProxySem 限制到 captcha-provider 的并发请求数（= WINDOW_POOL_SIZE）。
// 用带缓冲 channel 实现信号量：acquire 写入、release 读出。
// 懒初始化（首次使用时按 Cfg.FullProxyConcurrency 创建），避免依赖 config 初始化顺序。
var (
	fullProxySem    chan struct{}
	fullProxySemOnce sync.Once
)

func getFullProxySem() chan struct{} {
	fullProxySemOnce.Do(func() {
		n := 4
		if Cfg != nil && Cfg.FullProxyConcurrency > 0 {
			n = Cfg.FullProxyConcurrency
		}
		fullProxySem = make(chan struct{}, n)
		LogInfo("Full-proxy 并发限制: %d", n)
	})
	return fullProxySem
}

// isTransientError 判断错误是否为「瞬时/容量」类（重试 + 退避可能恢复）。
// MODEL_CONCURRENCY_LIMIT（模型容量满）、INTERNAL_ERROR（上游内部错误）属于此类。
// 这类错误换 token 无意义，但等一会再试可能恢复，所以重试时应加退避延迟。
func isTransientError(msg string) bool {
	m := strings.ToUpper(msg)
	return strings.Contains(m, "CONCURRENCY") ||
		strings.Contains(m, "CAPACITY") ||
		strings.Contains(m, "INTERNAL_ERROR") ||
		strings.Contains(m, "OVERLOAD") ||
		strings.Contains(m, "RATE_LIMIT")
}

// isCDNBlock 判断错误是否为「CDN 边缘拦截」（405/403）。
// 这类错误是 token 级别（阿里云 ESA CDN 对 URL query 里某些 JWT 拦截），
// 换 token 重试有效，且应标记当前 token 熔断。
// 错误格式形如："full-proxy /v1/chat status 405: <!doctypehtml..."
func isCDNBlock(msg string) bool {
	m := strings.ToUpper(msg)
	return strings.Contains(m, "STATUS 405") ||
		strings.Contains(m, "STATUS 403") ||
		strings.Contains(m, "STATUS:405") ||
		strings.Contains(m, "STATUS:403")
}

func (u *UpstreamData) GetEditContent() string {
	editContent := u.Data.EditContent
	if editContent == "" {
		return ""
	}

	if len(editContent) > 0 && editContent[0] == '"' {
		var unescaped string
		if err := json.Unmarshal([]byte(editContent), &unescaped); err == nil {
			LogDebug("[GetEditContent] Unescaped edit_content from JSON string")
			return unescaped
		}
	}

	return editContent
}

// cleanReasoningUTF8 清洗思考链（reasoning）里的乱码。
//
// 现象：z.ai 上游对 reasoning/thinking 内容做了某种后处理，导致个别中文字符被替换成
// 连续的 U+FFFD（替换符）。content 字段不受影响（恒为 0 个 FFFD），只有 reasoning 会坏。
// 实测损坏模式：每个被破坏的原字 → 2 个连续 FFFD（ef bf bd ef bf bd）。
//
// 处理：把任意连续的 FFFD 块替换成单个 □（U+25A1），既保证 UTF-8 合法，又保留可读性
// （读者知道这里原本有个字符）。同时清理掉残留的非法 UTF-8 字节。
func cleanReasoningUTF8(s string) string {
	if !strings.ContainsRune(s, '\uFFFD') {
		return s
	}
	// 把连续的 U+FFFD 压缩成单个 □
	var b strings.Builder
	b.Grow(len(s))
	runes := []rune(s)
	i := 0
	for i < len(runes) {
		if runes[i] == '\uFFFD' {
			// 跳过连续的 FFFD，输出一个 □
			for i < len(runes) && runes[i] == '\uFFFD' {
				i++
			}
			b.WriteRune('□')
		} else {
			b.WriteRune(runes[i])
			i++
		}
	}
	return b.String()
}

type ThinkingFilter struct {
	hasSeenFirstThinking bool
	buffer               string
	lastOutputChunk      string
	lastPhase            string
	thinkingRoundCount   int
}

func (f *ThinkingFilter) ProcessThinking(deltaContent string) string {
	if !f.hasSeenFirstThinking {
		// 合并缓存和当前内容，查找 "> " 作为思考内容的开始标记
		combined := f.buffer + deltaContent
		if idx := strings.Index(combined, "> "); idx != -1 {
			f.hasSeenFirstThinking = true
			f.buffer = ""
			deltaContent = combined[idx+2:]
		} else {
			// 没找到开始标记，缓存当前内容继续等待
			f.buffer = combined
			return ""
		}
	}

	content := f.buffer + deltaContent
	f.buffer = ""

	content = strings.ReplaceAll(content, "\n> ", "\n")

	if strings.HasSuffix(content, "\n>") {
		f.buffer = "\n>"
		return content[:len(content)-2]
	}
	if strings.HasSuffix(content, "\n") {
		f.buffer = "\n"
		return content[:len(content)-1]
	}

	return content
}

func (f *ThinkingFilter) Flush() string {
	result := f.buffer
	f.buffer = ""
	return result
}

func (f *ThinkingFilter) ExtractCompleteThinking(editContent string) string {
	startIdx := strings.Index(editContent, "> ")
	if startIdx == -1 {
		return ""
	}
	startIdx += 2

	endIdx := strings.Index(editContent, "\n</details>")
	if endIdx == -1 {
		return ""
	}

	content := editContent[startIdx:endIdx]
	content = strings.ReplaceAll(content, "\n> ", "\n")
	return content
}

func (f *ThinkingFilter) ExtractIncrementalThinking(editContent string) string {
	completeThinking := f.ExtractCompleteThinking(editContent)
	if completeThinking == "" {
		return ""
	}

	if f.lastOutputChunk == "" {
		return completeThinking
	}

	idx := strings.Index(completeThinking, f.lastOutputChunk)
	if idx == -1 {
		return completeThinking
	}

	incrementalPart := completeThinking[idx+len(f.lastOutputChunk):]
	return incrementalPart
}

func (f *ThinkingFilter) ResetForNewRound() {
	f.lastOutputChunk = ""
	f.hasSeenFirstThinking = false
}

func getUpstreamToken() (string, error) {
	// 优先使用 TokenManager 中的用户 token，其次 BACKUP_TOKEN，最后匿名 token。
	if tmToken := GetTokenManager().GetToken(); tmToken != "" {
		LogDebug("Using token from TokenManager")
		return tmToken, nil
	}
	if backupToken := GetBackupToken(); backupToken != "" {
		LogDebug("Using backup token")
		return backupToken, nil
	}
	anonymousToken, err := GetAnonymousToken()
	if err != nil {
		return "", err
	}
	LogDebug("Using anonymous token: %s...", anonymousToken[:min(10, len(anonymousToken))])
	return anonymousToken, nil
}

func HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// 只接受 POST 请求
	if r.Method != http.MethodPost {
		writeInvalidRequestError(w, "Only POST method is allowed")
		return
	}

	apiKey := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	// API Key 认证
	if !Cfg.SkipAuthToken {
		if apiKey == "" {
			LogDebug("Missing Authorization header")
			writeError(w, http.StatusUnauthorized, ErrTypeAuthentication, "Missing or invalid Authorization header", "invalid_api_key")
			return
		}
		// 验证 API Key
		if !ValidateAuthToken(apiKey) {
			LogDebug("Invalid API key: %s...", apiKey[:min(8, len(apiKey))])
			writeError(w, http.StatusUnauthorized, ErrTypeAuthentication, "Invalid API key", "invalid_api_key")
			return
		}
		LogDebug("API key validated: %s...", apiKey[:min(8, len(apiKey))])
	} else {
		LogDebug("SKIP_AUTH_TOKEN enabled, skipping API key validation")
	}
	// 先解析请求，避免在无效请求上消耗/轮询上游 token
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeInvalidRequestError(w, "无效的请求格式")
		return
	}

	if req.Model == "" {
		req.Model = "GLM-4.6"
	}

	// 验证模型是否存在
	if !IsValidModel(req.Model) {
		writeModelNotFoundError(w, req.Model)
		return
	}

	clientIP := GetClientIP(r)
	isMultimodal := false
	// 检测多模态
	reqImageURLs, reqVideoURLs := extractAllMediaURLs(req.Messages)
	if len(reqImageURLs) > 0 || len(reqVideoURLs) > 0 {
		isMultimodal = true
		LogDebug("[Request] Multimodal detected: images=%d, videos=%d", len(reqImageURLs), len(reqVideoURLs))
		for i, url := range reqImageURLs {
			urlPreview := url
			if len(urlPreview) > 80 {
				urlPreview = urlPreview[:80] + "..."
			}
			LogDebug("[Request] Image %d: %s", i+1, urlPreview)
		}
		for i, url := range reqVideoURLs {
			urlPreview := url
			if len(urlPreview) > 80 {
				urlPreview = urlPreview[:80] + "..."
			}
			LogDebug("[Request] Video %d: %s", i+1, urlPreview)
		}
	}

	// 处理工具调用
	messages := req.Messages
	// Agent 模式由 z.ai 上游原生支持工具调用，不需要 prompt injection
	// 只在传统 chat 模式（无 agent flag）时才注入工具 prompt
	// 工具调用：通过 prompt injection 让模型输出 XML 工具调用，
	// 我们在响应里解析回 OpenAI 格式 tool_calls。
	// 系统提示词：z.ai 会覆盖客户端的 role:system 消息，导致 system prompt 丢失。
	// 把所有 system 消息提取出来，拼到第一条 user 消息前面（z.ai 保留 user 消息）。
	messages = mergeSystemIntoFirstUser(messages)

	if len(req.Tools) > 0 {
		// FORCE_TOOL_CHOICE_REQUIRED：把没指定 tool_choice / auto 的请求升级为 required
		toolChoice := req.ToolChoice
		if Cfg.ForceToolChoiceRequired {
			if tc, ok := toolChoice.(string); !ok || tc == "" || tc == "auto" {
				toolChoice = "required"
				LogDebug("[Tools] Forced tool_choice to 'required'")
			}
		}
		messages = ProcessMessagesWithTools(messages, req.Tools, toolChoice)
	}

	inputTokens := CountRequestTokens(messages, req.Tools)
	LogDebug("Chat request: model=%s, messages=%d, stream=%v, input_tokens=%d, ip=%s, multimodal=%v, tools=%d",
		req.Model, len(messages), req.Stream, inputTokens, clientIP, isMultimodal, len(req.Tools))

	completionID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:29])
	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	reqStartedAt := time.Now()

	var outputTokens int64
	var lastError string
	success := false
	var token string

	// 全代理路径并发信号量：限制到 provider 的并发数（= WINDOW_POOL_SIZE）。
	// 超出的请求在此排队（带缓冲 channel），避免 provider 窗口池耗尽后大面积超时。
	// 必须覆盖整个请求生命周期（含流式消费），所以 defer 到函数结束才释放。
	if Cfg.CaptchaFullProxyURL != "" {
		sem := getFullProxySem()
		sem <- struct{}{}        // acquire（满则阻塞排队）
		defer func() { <-sem }() // release（函数返回时）
	}

	// 重试循环
	maxRetries := max(Cfg.RetryCount, 0)
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			LogInfo("Retry %d/%d with next available token", attempt, maxRetries)
		}

		var tokenErr error
		token, tokenErr = getUpstreamToken()
		if tokenErr != nil {
			LogError("Failed to get upstream token (attempt %d): %v", attempt+1, tokenErr)
			lastError = tokenErr.Error()
			break
		}

		resp, modelName, err := makeUpstreamRequest(token, messages, req.Model, reqImageURLs, reqVideoURLs, len(req.Tools) > 0, req.Tools, req.ReasoningEffort)
		if err != nil {
			LogError("Upstream request failed (attempt %d): %v", attempt+1, err)
			lastError = err.Error()
			// 405/403 = 阿里云 ESA CDN 边缘拦截。实测是间歇性的（好 token 也偶发），
			// 换 token 最终能成功。两个动作：
			//   1) 累积计数（连续多次才熔断，避免误杀好 token）——用于后台展示 token 质量
			//   2) 退避后重试（给 CDN 限流窗口冷却，降低下次撞 405 概率）
			if isCDNBlock(lastError) {
				GetTokenManager().MarkTokenBlocked(token, "405/403 CDN拦截")
				if attempt < maxRetries {
					backoff := time.Duration(attempt+1) * 1500 * time.Millisecond // 1.5s, 3s, 4.5s...
					if backoff > 8*time.Second {
						backoff = 8 * time.Second
					}
					LogInfo("CDN 405, backing off %v before retry %d/%d", backoff, attempt+2, maxRetries+1)
					time.Sleep(backoff)
				}
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			LogError("Upstream error (attempt %d): status=%d, body=%s", attempt+1, resp.StatusCode, string(body)[:min(500, len(body))])
			lastError = fmt.Sprintf("status %d", resp.StatusCode)
			// 429 限流也属于瞬时错误，允许重试（5xx 本来就重试）
			isRetryableStatus := resp.StatusCode >= 500 || resp.StatusCode == 429
			if !isRetryableStatus {
				GetTokenManager().RecordCall(false, isMultimodal)
				RecordUsageFull(storageUsageRecord{
					ApiKey: apiKey, Model: req.Model, InputTok: inputTokens,
					OutputTok: 0, Success: false, IsMultimodal: isMultimodal,
					LatencyMs: int(time.Since(reqStartedAt).Milliseconds()),
				})
				writeUpstreamError(w, resp.StatusCode, body)
				return
			}
			// 5xx/429 退避后重试
			if attempt < maxRetries {
				backoff := time.Duration(attempt+1) * 1500 * time.Millisecond
				if backoff > 6*time.Second {
					backoff = 6 * time.Second
				}
				LogInfo("Status %d, backing off %v before retry %d/%d", resp.StatusCode, backoff, attempt+2, maxRetries+1)
				time.Sleep(backoff)
			}
			continue
		}

		var result UpstreamResult
		if req.Stream {
			result = handleStreamResponseWithRetry(w, resp.Body, completionID, modelName, inputTokens, includeUsage, req.Tools, attempt == 0)
		} else {
			result = handleNonStreamResponseWithRetry(w, resp.Body, completionID, modelName, inputTokens, req.Tools)
		}
		resp.Body.Close()

		outputTokens = result.OutputTokens

		if result.Success && result.HasContent {
			success = true
			// 请求成功：清零该 token 的连续失败计数（间歇 405 后恢复）。
			GetTokenManager().MarkTokenSuccess(token)
			break
		}

		// 检查是否需要重试
		if result.ErrorMessage != "" {
			lastError = result.ErrorMessage
			LogWarn("Upstream returned error (attempt %d): %s", attempt+1, result.ErrorMessage)
		} else if !result.HasContent {
			lastError = "empty response"
			LogWarn("Upstream returned empty content (attempt %d)", attempt+1)
		}

		// 流式请求已开始写入，无法重试
		if req.Stream && result.ResponseStarted {
			LogDebug("Stream response already started, cannot retry")
			break
		}

		// 瞬时错误（模型容量满/内部错误/限流）退避重试：等一会再试可能恢复
		if isTransientError(lastError) && attempt < maxRetries {
			backoff := time.Duration(attempt+1) * 1500 * time.Millisecond // 1.5s, 3s, 4.5s...
			if backoff > 6*time.Second {
				backoff = 6 * time.Second
			}
			LogInfo("Transient error, backing off %v before retry %d/%d", backoff, attempt+2, maxRetries+1)
			time.Sleep(backoff)
		}
	}

	if !success && !req.Stream {
		// 非流式请求失败，返回错误
		GetTokenManager().RecordCall(false, isMultimodal)
		RecordUsageFull(storageUsageRecord{
			ApiKey: apiKey, Model: req.Model, InputTok: inputTokens,
			OutputTok: outputTokens, Success: false, IsMultimodal: isMultimodal,
			LatencyMs: int(time.Since(reqStartedAt).Milliseconds()),
		})
		writeError(w, http.StatusBadGateway, ErrTypeUpstream, fmt.Sprintf("请求失败: %s", lastError), "upstream_error")
		return
	}

	// 记录遥测数据
	RecordRequest(inputTokens, outputTokens, req.Model)
	GetTokenManager().RecordCall(success, isMultimodal)
	RecordUsageFull(storageUsageRecord{
		ApiKey: apiKey, Model: req.Model, InputTok: inputTokens,
		OutputTok: outputTokens, Success: success, IsMultimodal: isMultimodal,
		LatencyMs: int(time.Since(reqStartedAt).Milliseconds()),
	})
	LogDebug("Chat completed: model=%s, input_tokens=%d, output_tokens=%d, ip=%s, success=%v",
		req.Model, inputTokens, outputTokens, clientIP, success)
}

func handleStreamResponse(w http.ResponseWriter, body io.ReadCloser, completionID, modelName string, inputTokens int64, includeUsage bool, tools []Tool) int64 {
	var outputTokens int64
	var fullContent strings.Builder
	var fullReasoning strings.Builder // 累积思考链内容，工具调用回退解析用
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return 0
	}

	// 发送第一个 chunk 带 role
	firstChunk := ChatCompletionChunk{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []Choice{{
			Index:        0,
			Delta:        &Delta{Role: "assistant"},
			FinishReason: nil,
		}},
	}
	data, _ := json.Marshal(firstChunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	hasContent := false
	searchRefFilter := NewSearchRefFilter()
	thinkingFilter := &ThinkingFilter{}
	pendingSourcesMarkdown := ""
	pendingImageSearchMarkdown := ""
	totalContentOutputLength := 0 // 记录已输出的 content 字符长度
	hasTools := len(tools) > 0
	diagHexDumped := false // 诊断用：只 dump 第一条 thinking 行的原始 hex

	for scanner.Scan() {
		line := scanner.Text()
		LogDebug("[Upstream] %s", line)

		// 诊断：DEBUG 时，对第一条 thinking 行 dump 原始 hex，定位上游字节是否在到达
		// scanner 前就已损坏（区分是 tls-client/代理 还是 后续 json 解析的问题）。
		if Cfg.DebugLogging && !diagHexDumped && strings.Contains(line, `"phase":"thinking"`) {
			diagHexDumped = true
			hb := []byte(line)
			n := len(hb)
			if n > 400 {
				n = 400
			}
			LogDebug("[Diag][UpstreamRawHex] len=%d first%d=%x", len(line), n, hb[:n])
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var upstream UpstreamData
		if err := json.Unmarshal([]byte(payload), &upstream); err != nil {
			continue
		}

		// 检测上游错误
		if upstream.HasError() {
			LogError("Upstream error: %s", upstream.GetErrorMessage())
			errContent := fmt.Sprintf("[上游服务错误: %s]", upstream.GetErrorMessage())
			errChunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        &Delta{Content: errContent},
					FinishReason: nil,
				}},
			}
			errData, _ := json.Marshal(errChunk)
			fmt.Fprintf(w, "data: %s\n\n", errData)
			flusher.Flush()
			hasContent = true
			break
		}

		if upstream.Data.Phase == "done" {
			break
		}

		if upstream.Data.Phase == "thinking" && upstream.Data.DeltaContent != "" {
			isNewThinkingRound := false
			if thinkingFilter.lastPhase != "" && thinkingFilter.lastPhase != "thinking" {
				thinkingFilter.ResetForNewRound()
				thinkingFilter.thinkingRoundCount++
				isNewThinkingRound = true
			}
			thinkingFilter.lastPhase = "thinking"

			reasoningContent := cleanReasoningUTF8(thinkingFilter.ProcessThinking(upstream.Data.DeltaContent))

			if isNewThinkingRound && thinkingFilter.thinkingRoundCount > 1 && reasoningContent != "" {
				reasoningContent = "\n\n" + reasoningContent
			}

			if reasoningContent != "" {
				thinkingFilter.lastOutputChunk = reasoningContent
				reasoningContent = searchRefFilter.Process(reasoningContent)

				if reasoningContent != "" {
					hasContent = true
					fullReasoning.WriteString(reasoningContent) // 累积用于工具调用回退解析
					chunk := ChatCompletionChunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   modelName,
						Choices: []Choice{{
							Index:        0,
							Delta:        &Delta{ReasoningContent: reasoningContent},
							FinishReason: nil,
						}},
					}
					data, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
			}
			continue
		}

		if upstream.Data.Phase != "" {
			thinkingFilter.lastPhase = upstream.Data.Phase
		}

		editContent := upstream.GetEditContent()
		if editContent != "" && IsSearchResultContent(editContent) {
			if results := ParseSearchResults(editContent); len(results) > 0 {
				searchRefFilter.AddSearchResults(results)
				pendingSourcesMarkdown = searchRefFilter.GetSearchResultsMarkdown()
			}
			continue
		}
		if editContent != "" && strings.Contains(editContent, `"search_image"`) {
			textBeforeBlock := ExtractTextBeforeGlmBlock(editContent)
			if textBeforeBlock != "" {
				textBeforeBlock = searchRefFilter.Process(textBeforeBlock)
				if textBeforeBlock != "" {
					hasContent = true
					chunk := ChatCompletionChunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   modelName,
						Choices: []Choice{{
							Index:        0,
							Delta:        &Delta{Content: textBeforeBlock},
							FinishReason: nil,
						}},
					}
					data, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
			}
			if results := ParseImageSearchResults(editContent); len(results) > 0 {
				pendingImageSearchMarkdown = FormatImageSearchResults(results)
			}
			continue
		}
		if editContent != "" && strings.Contains(editContent, `"mcp"`) {
			textBeforeBlock := ExtractTextBeforeGlmBlock(editContent)
			if textBeforeBlock != "" {
				textBeforeBlock = searchRefFilter.Process(textBeforeBlock)
				if textBeforeBlock != "" {
					hasContent = true
					chunk := ChatCompletionChunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   modelName,
						Choices: []Choice{{
							Index:        0,
							Delta:        &Delta{Content: textBeforeBlock},
							FinishReason: nil,
						}},
					}
					data, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
			}
			continue
		}
		if editContent != "" && IsSearchToolCall(editContent, upstream.Data.Phase) {
			continue
		}

		if pendingSourcesMarkdown != "" {
			hasContent = true
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        &Delta{Content: pendingSourcesMarkdown},
					FinishReason: nil,
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			pendingSourcesMarkdown = ""
		}
		if pendingImageSearchMarkdown != "" {
			hasContent = true
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        &Delta{Content: pendingImageSearchMarkdown},
					FinishReason: nil,
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			pendingImageSearchMarkdown = ""
		}

		content := ""
		reasoningContent := ""

		if thinkingRemaining := thinkingFilter.Flush(); thinkingRemaining != "" {
			thinkingFilter.lastOutputChunk = thinkingRemaining
			processedRemaining := searchRefFilter.Process(thinkingRemaining)
			if processedRemaining != "" {
				hasContent = true
				chunk := ChatCompletionChunk{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   modelName,
					Choices: []Choice{{
						Index:        0,
						Delta:        &Delta{ReasoningContent: processedRemaining},
						FinishReason: nil,
					}},
				}
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
		}

		if pendingSourcesMarkdown != "" && thinkingFilter.hasSeenFirstThinking {
			hasContent = true
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        &Delta{ReasoningContent: pendingSourcesMarkdown},
					FinishReason: nil,
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			pendingSourcesMarkdown = ""
		}

		if upstream.Data.Phase == "answer" && upstream.Data.DeltaContent != "" {
			content = upstream.Data.DeltaContent
		} else if upstream.Data.Phase == "answer" && editContent != "" {
			if strings.Contains(editContent, "</details>") {
				reasoningContent = thinkingFilter.ExtractIncrementalThinking(editContent)

				if idx := strings.Index(editContent, "</details>"); idx != -1 {
					afterDetails := editContent[idx+len("</details>"):]
					if strings.HasPrefix(afterDetails, "\n") {
						content = afterDetails[1:]
					} else {
						content = afterDetails
					}
					totalContentOutputLength = len([]rune(content))
				}
			}
		} else if (upstream.Data.Phase == "other" || upstream.Data.Phase == "tool_call") && editContent != "" {
			fullContent := editContent
			fullContentRunes := []rune(fullContent)

			if len(fullContentRunes) > totalContentOutputLength {
				content = string(fullContentRunes[totalContentOutputLength:])
				totalContentOutputLength = len(fullContentRunes)
			} else {
				content = fullContent
			}
		}

		if reasoningContent != "" {
			reasoningContent = searchRefFilter.Process(reasoningContent) + searchRefFilter.Flush()
		}
		if reasoningContent != "" {
			hasContent = true
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        &Delta{ReasoningContent: reasoningContent},
					FinishReason: nil,
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		if content == "" {
			continue
		}

		content = searchRefFilter.Process(content)
		if content == "" {
			continue
		}

		hasContent = true
		if upstream.Data.Phase == "answer" && upstream.Data.DeltaContent != "" {
			totalContentOutputLength += len([]rune(content))
		}
		fullContent.WriteString(content)
		outputTokens += CountTokens(content)
		if hasTools {
			continue
		}

		chunk := ChatCompletionChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   modelName,
			Choices: []Choice{{
				Index:        0,
				Delta:        &Delta{Content: content},
				FinishReason: nil,
			}},
		}

		chunkData, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", chunkData)
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		LogError("[Upstream] scanner error: %v", err)
	}

	if remaining := searchRefFilter.Flush(); remaining != "" {
		hasContent = true
		fullContent.WriteString(remaining)
		if !hasTools {
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        &Delta{Content: remaining},
					FinishReason: nil,
				}},
			}
			chunkData, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", chunkData)
			flusher.Flush()
		}
	}

	if !hasContent {
		LogError("Stream response 200 but no content received")
	}
	stopReason := "stop"
	var toolCalls []ToolCall
	if len(tools) > 0 {
		rawContent := fullContent.String()
		toolCalls = ExtractToolInvocationsWithFallback(rawContent, fullReasoning.String())
		if len(toolCalls) > 0 {
			stopReason = "tool_calls"
			LogDebug("[Stream] Detected %d tool calls, sending tool_calls chunks", len(toolCalls))
			for i, tc := range toolCalls {
				if tc.ID == "" {
					tc.ID = generateCallID()
				}
				toolChunk := ChatCompletionChunk{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   modelName,
					Choices: []Choice{{
						Index: 0,
						Delta: &Delta{
							ToolCalls: []ToolCall{{
								Index:    i,
								ID:       tc.ID,
								Type:     tc.Type,
								Function: tc.Function,
							}},
						},
						FinishReason: nil,
					}},
				}
				toolData, _ := json.Marshal(toolChunk)
				fmt.Fprintf(w, "data: %s\n\n", toolData)
				flusher.Flush()
			}
		} else {
			bufferedContent := RemoveToolJSONContent(rawContent)
			if bufferedContent != "" {
				chunk := ChatCompletionChunk{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   modelName,
					Choices: []Choice{{
						Index:        0,
						Delta:        &Delta{Content: bufferedContent},
						FinishReason: nil,
					}},
				}
				chunkData, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", chunkData)
				flusher.Flush()
			}
		}
	}

	finalChunk := ChatCompletionChunk{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []Choice{{
			Index:        0,
			Delta:        &Delta{},
			FinishReason: &stopReason,
		}},
	}

	finalData, _ := json.Marshal(finalChunk)
	fmt.Fprintf(w, "data: %s\n\n", finalData)
	if includeUsage {
		usageChunk := ChatCompletionChunkResponse{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   modelName,
			Choices: []Choice{},
			Usage: &Usage{
				PromptTokens:     inputTokens,
				CompletionTokens: outputTokens,
				TotalTokens:      inputTokens + outputTokens,
			},
		}
		usageData, _ := json.Marshal(usageChunk)
		fmt.Fprintf(w, "data: %s\n\n", usageData)
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
	return outputTokens
}

func handleNonStreamResponse(w http.ResponseWriter, body io.ReadCloser, completionID, modelName string, inputTokens int64, tools []Tool) int64 {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", completionID)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	var outputTokens int64
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var chunks []string
	var reasoningChunks []string
	thinkingFilter := &ThinkingFilter{}
	searchRefFilter := NewSearchRefFilter()
	hasThinking := false
	diagHexDumped := false // 诊断用：只 dump 第一条 thinking 行的原始 hex
	pendingSourcesMarkdown := ""
	pendingImageSearchMarkdown := ""

	for scanner.Scan() {
		line := scanner.Text()
		LogDebug("[Upstream] %s", line)

		// 诊断：DEBUG 时，对第一条 thinking 行 dump 原始 hex，定位上游字节是否在到达
		// scanner 前就已损坏（区分是 tls-client/代理 还是 后续 json 解析的问题）。
		if Cfg.DebugLogging && !diagHexDumped && strings.Contains(line, `"phase":"thinking"`) {
			diagHexDumped = true
			hb := []byte(line)
			n := len(hb)
			if n > 400 {
				n = 400
			}
			LogDebug("[Diag][UpstreamRawHex] len=%d first%d=%x", len(line), n, hb[:n])
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var upstream UpstreamData
		if err := json.Unmarshal([]byte(payload), &upstream); err != nil {
			continue
		}
		if upstream.HasError() {
			LogError("Upstream error: %s", upstream.GetErrorMessage())
			chunks = append(chunks, fmt.Sprintf("[上游服务错误: %s]", upstream.GetErrorMessage()))
			break
		}

		if upstream.Data.Phase == "done" {
			break
		}

		if upstream.Data.Phase == "thinking" && upstream.Data.DeltaContent != "" {
			if thinkingFilter.lastPhase != "" && thinkingFilter.lastPhase != "thinking" {
				thinkingFilter.ResetForNewRound()
				thinkingFilter.thinkingRoundCount++
				if thinkingFilter.thinkingRoundCount > 1 {
					reasoningChunks = append(reasoningChunks, "\n\n")
				}
			}
			thinkingFilter.lastPhase = "thinking"

			hasThinking = true
			reasoningContent := cleanReasoningUTF8(thinkingFilter.ProcessThinking(upstream.Data.DeltaContent))
			if reasoningContent != "" {
				thinkingFilter.lastOutputChunk = reasoningContent
				reasoningChunks = append(reasoningChunks, reasoningContent)
			}
			continue
		}

		if upstream.Data.Phase != "" {
			thinkingFilter.lastPhase = upstream.Data.Phase
		}

		editContent := upstream.GetEditContent()
		if editContent != "" && IsSearchResultContent(editContent) {
			if results := ParseSearchResults(editContent); len(results) > 0 {
				searchRefFilter.AddSearchResults(results)
				pendingSourcesMarkdown = searchRefFilter.GetSearchResultsMarkdown()
			}
			continue
		}
		if editContent != "" && strings.Contains(editContent, `"search_image"`) {
			textBeforeBlock := ExtractTextBeforeGlmBlock(editContent)
			if textBeforeBlock != "" {
				chunks = append(chunks, textBeforeBlock)
			}
			if results := ParseImageSearchResults(editContent); len(results) > 0 {
				pendingImageSearchMarkdown = FormatImageSearchResults(results)
			}
			continue
		}
		if editContent != "" && strings.Contains(editContent, `"mcp"`) {
			textBeforeBlock := ExtractTextBeforeGlmBlock(editContent)
			if textBeforeBlock != "" {
				chunks = append(chunks, textBeforeBlock)
			}
			continue
		}
		if editContent != "" && IsSearchToolCall(editContent, upstream.Data.Phase) {
			continue
		}

		if pendingSourcesMarkdown != "" {
			if hasThinking {
				reasoningChunks = append(reasoningChunks, pendingSourcesMarkdown)
			} else {
				chunks = append(chunks, pendingSourcesMarkdown)
			}
			pendingSourcesMarkdown = ""
		}
		if pendingImageSearchMarkdown != "" {
			chunks = append(chunks, pendingImageSearchMarkdown)
			pendingImageSearchMarkdown = ""
		}

		content := ""
		if upstream.Data.Phase == "answer" && upstream.Data.DeltaContent != "" {
			content = upstream.Data.DeltaContent
		} else if upstream.Data.Phase == "answer" && editContent != "" {
			if strings.Contains(editContent, "</details>") {
				reasoningContent := cleanReasoningUTF8(thinkingFilter.ExtractIncrementalThinking(editContent))
				if reasoningContent != "" {
					reasoningChunks = append(reasoningChunks, reasoningContent)
				}

				if idx := strings.Index(editContent, "</details>"); idx != -1 {
					afterDetails := editContent[idx+len("</details>"):]
					if strings.HasPrefix(afterDetails, "\n") {
						content = afterDetails[1:]
					} else {
						content = afterDetails
					}
				}
			}
		} else if (upstream.Data.Phase == "other" || upstream.Data.Phase == "tool_call") && editContent != "" {
			content = editContent
		}

		if content != "" {
			chunks = append(chunks, content)
		}
	}

	fullContent := strings.Join(chunks, "")
	fullContent = searchRefFilter.Process(fullContent) + searchRefFilter.Flush()
	fullReasoning := strings.Join(reasoningChunks, "")
	fullReasoning = searchRefFilter.Process(fullReasoning) + searchRefFilter.Flush()

	if fullContent == "" && fullReasoning == "" {
		LogError("Non-stream response 200 but no content received")
	}
	stopReason := "stop"
	var toolCalls []ToolCall
	if len(tools) > 0 {
		toolCalls = ExtractToolInvocationsWithFallback(fullContent, fullReasoning)
		fullContent = RemoveToolJSONContent(fullContent)
		if len(toolCalls) > 0 {
			stopReason = "tool_calls"
		}
	}
	outputTokens = CountTokens(fullContent) + CountTokens(fullReasoning)

	response := ChatCompletionResponse{
		ID:      completionID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []Choice{{
			Index: 0,
			Message: &MessageResp{
				Role:             "assistant",
				Content:          fullContent,
				ReasoningContent: fullReasoning,
				ToolCalls:        toolCalls,
			},
			FinishReason: &stopReason,
		}},
		Usage: &Usage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		},
		SystemFingerprint: "openai",
	}
	json.NewEncoder(w).Encode(response)
	return outputTokens
}
func handleStreamResponseWithRetry(w http.ResponseWriter, body io.ReadCloser, completionID, modelName string, inputTokens int64, includeUsage bool, tools []Tool, isFirstAttempt bool) UpstreamResult {
	result := UpstreamResult{Success: true, HasContent: false}
	var outputTokens int64
	var fullContent strings.Builder
	var fullReasoning strings.Builder // 累积思考链内容，工具调用回退解析用
	var upstreamError string

	flusher, ok := w.(http.Flusher)
	if !ok {
		result.Success = false
		result.ErrorMessage = "streaming not supported"
		return result
	}

	startStream := func() error {
		if result.ResponseStarted {
			return nil
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		firstChunk := ChatCompletionChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   modelName,
			Choices: []Choice{{
				Index:        0,
				Delta:        &Delta{Role: "assistant"},
				FinishReason: nil,
			}},
		}
		data, _ := json.Marshal(firstChunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		flusher.Flush()
		result.ResponseStarted = true
		return nil
	}

	sendChunk := func(chunk interface{}) bool {
		if err := startStream(); err != nil {
			result.Success = false
			result.ErrorMessage = err.Error()
			return false
		}
		chunkData, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", chunkData); err != nil {
			result.Success = false
			result.ErrorMessage = err.Error()
			return false
		}
		flusher.Flush()
		return true
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	hasContent := false
	searchRefFilter := NewSearchRefFilter()
	thinkingFilter := &ThinkingFilter{}
	pendingSourcesMarkdown := ""
	pendingImageSearchMarkdown := ""
	totalContentOutputLength := 0
	hasTools := len(tools) > 0
	diagHexDumped := false // 诊断用：只 dump 第一条 thinking 行的原始 hex

	for scanner.Scan() {
		line := scanner.Text()
		LogDebug("[Upstream] %s", line)

		// 诊断：DEBUG 时，对第一条 thinking 行 dump 原始 hex，定位上游字节是否在到达
		// scanner 前就已损坏（区分是 tls-client/代理 还是 后续 json 解析的问题）。
		if Cfg.DebugLogging && !diagHexDumped && strings.Contains(line, `"phase":"thinking"`) {
			diagHexDumped = true
			hb := []byte(line)
			n := len(hb)
			if n > 400 {
				n = 400
			}
			LogDebug("[Diag][UpstreamRawHex] len=%d first%d=%x", len(line), n, hb[:n])
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var upstream UpstreamData
		if err := json.Unmarshal([]byte(payload), &upstream); err != nil {
			continue
		}
		if upstream.HasError() {
			upstreamError = upstream.GetErrorMessage()
			LogError("Upstream error: %s", upstreamError)
			result.Success = false
			result.ErrorMessage = upstreamError
			if result.ResponseStarted {
				errContent := fmt.Sprintf("[上游服务错误: %s]", upstreamError)
				errChunk := ChatCompletionChunk{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   modelName,
					Choices: []Choice{{
						Index:        0,
						Delta:        &Delta{Content: errContent},
						FinishReason: nil,
					}},
				}
				if !sendChunk(errChunk) {
					return result
				}
				hasContent = true
			}
			break
		}

		if upstream.Data.Phase == "done" {
			break
		}

		if upstream.Data.Phase == "thinking" && upstream.Data.DeltaContent != "" {
			isNewThinkingRound := false
			if thinkingFilter.lastPhase != "" && thinkingFilter.lastPhase != "thinking" {
				thinkingFilter.ResetForNewRound()
				thinkingFilter.thinkingRoundCount++
				isNewThinkingRound = true
			}
			thinkingFilter.lastPhase = "thinking"

			reasoningContent := cleanReasoningUTF8(thinkingFilter.ProcessThinking(upstream.Data.DeltaContent))

			if isNewThinkingRound && thinkingFilter.thinkingRoundCount > 1 && reasoningContent != "" {
				reasoningContent = "\n\n" + reasoningContent
			}

			if reasoningContent != "" {
				thinkingFilter.lastOutputChunk = reasoningContent
				reasoningContent = searchRefFilter.Process(reasoningContent)

				if reasoningContent != "" {
					hasContent = true
					fullReasoning.WriteString(reasoningContent) // 累积用于工具调用回退解析
					chunk := ChatCompletionChunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   modelName,
						Choices: []Choice{{
							Index:        0,
							Delta:        &Delta{ReasoningContent: reasoningContent},
							FinishReason: nil,
						}},
					}
					if !sendChunk(chunk) {
						return result
					}
				}
			}
			continue
		}

		if upstream.Data.Phase != "" {
			thinkingFilter.lastPhase = upstream.Data.Phase
		}

		editContent := upstream.GetEditContent()
		if editContent != "" && IsSearchResultContent(editContent) {
			if results := ParseSearchResults(editContent); len(results) > 0 {
				searchRefFilter.AddSearchResults(results)
				pendingSourcesMarkdown = searchRefFilter.GetSearchResultsMarkdown()
			}
			continue
		}
		if editContent != "" && strings.Contains(editContent, `"search_image"`) {
			textBeforeBlock := ExtractTextBeforeGlmBlock(editContent)
			if textBeforeBlock != "" {
				textBeforeBlock = searchRefFilter.Process(textBeforeBlock)
				if textBeforeBlock != "" {
					hasContent = true
					chunk := ChatCompletionChunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   modelName,
						Choices: []Choice{{
							Index:        0,
							Delta:        &Delta{Content: textBeforeBlock},
							FinishReason: nil,
						}},
					}
					if !sendChunk(chunk) {
						return result
					}
				}
			}
			if results := ParseImageSearchResults(editContent); len(results) > 0 {
				pendingImageSearchMarkdown = FormatImageSearchResults(results)
			}
			continue
		}
		if editContent != "" && strings.Contains(editContent, `"mcp"`) {
			textBeforeBlock := ExtractTextBeforeGlmBlock(editContent)
			if textBeforeBlock != "" {
				textBeforeBlock = searchRefFilter.Process(textBeforeBlock)
				if textBeforeBlock != "" {
					hasContent = true
					chunk := ChatCompletionChunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   modelName,
						Choices: []Choice{{
							Index:        0,
							Delta:        &Delta{Content: textBeforeBlock},
							FinishReason: nil,
						}},
					}
					if !sendChunk(chunk) {
						return result
					}
				}
			}
			continue
		}
		if editContent != "" && IsSearchToolCall(editContent, upstream.Data.Phase) {
			continue
		}

		if pendingSourcesMarkdown != "" {
			hasContent = true
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        &Delta{Content: pendingSourcesMarkdown},
					FinishReason: nil,
				}},
			}
			if !sendChunk(chunk) {
				return result
			}
			pendingSourcesMarkdown = ""
		}
		if pendingImageSearchMarkdown != "" {
			hasContent = true
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        &Delta{Content: pendingImageSearchMarkdown},
					FinishReason: nil,
				}},
			}
			if !sendChunk(chunk) {
				return result
			}
			pendingImageSearchMarkdown = ""
		}

		content := ""
		reasoningContent := ""

		if thinkingRemaining := thinkingFilter.Flush(); thinkingRemaining != "" {
			thinkingFilter.lastOutputChunk = thinkingRemaining
			processedRemaining := searchRefFilter.Process(thinkingRemaining)
			if processedRemaining != "" {
				hasContent = true
				chunk := ChatCompletionChunk{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   modelName,
					Choices: []Choice{{
						Index:        0,
						Delta:        &Delta{ReasoningContent: processedRemaining},
						FinishReason: nil,
					}},
				}
				if !sendChunk(chunk) {
					return result
				}
			}
		}

		if pendingSourcesMarkdown != "" && thinkingFilter.hasSeenFirstThinking {
			hasContent = true
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        &Delta{ReasoningContent: pendingSourcesMarkdown},
					FinishReason: nil,
				}},
			}
			if !sendChunk(chunk) {
				return result
			}
			pendingSourcesMarkdown = ""
		}

		if upstream.Data.Phase == "answer" && upstream.Data.DeltaContent != "" {
			content = upstream.Data.DeltaContent
		} else if upstream.Data.Phase == "answer" && editContent != "" {
			if strings.Contains(editContent, "</details>") {
				reasoningContent = thinkingFilter.ExtractIncrementalThinking(editContent)

				if idx := strings.Index(editContent, "</details>"); idx != -1 {
					afterDetails := editContent[idx+len("</details>"):]
					if strings.HasPrefix(afterDetails, "\n") {
						content = afterDetails[1:]
					} else {
						content = afterDetails
					}
					totalContentOutputLength = len([]rune(content))
				}
			}
		} else if (upstream.Data.Phase == "other" || upstream.Data.Phase == "tool_call") && editContent != "" {
			fullEditContent := editContent
			fullContentRunes := []rune(fullEditContent)

			if len(fullContentRunes) > totalContentOutputLength {
				content = string(fullContentRunes[totalContentOutputLength:])
				totalContentOutputLength = len(fullContentRunes)
			} else {
				content = fullEditContent
			}
		}

		if reasoningContent != "" {
			reasoningContent = searchRefFilter.Process(reasoningContent) + searchRefFilter.Flush()
		}
		if reasoningContent != "" {
			hasContent = true
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        &Delta{ReasoningContent: reasoningContent},
					FinishReason: nil,
				}},
			}
			if !sendChunk(chunk) {
				return result
			}
		}

		if content == "" {
			continue
		}

		content = searchRefFilter.Process(content)
		if content == "" {
			continue
		}

		hasContent = true
		if upstream.Data.Phase == "answer" && upstream.Data.DeltaContent != "" {
			totalContentOutputLength += len([]rune(content))
		}
		fullContent.WriteString(content)
		if hasTools {
			outputTokens += CountTokens(content)
			continue
		}

		chunk := ChatCompletionChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   modelName,
			Choices: []Choice{{
				Index:        0,
				Delta:        &Delta{Content: content},
				FinishReason: nil,
			}},
		}

		outputTokens += CountTokens(content)
		if !sendChunk(chunk) {
			return result
		}
	}

	if remaining := searchRefFilter.Flush(); remaining != "" {
		hasContent = true
		fullContent.WriteString(remaining)
		if !hasTools {
			chunk := ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   modelName,
				Choices: []Choice{{
					Index:        0,
					Delta:        &Delta{Content: remaining},
					FinishReason: nil,
				}},
			}
			if !sendChunk(chunk) {
				return result
			}
		}
	}
	stopReason := "stop"
	var toolCalls []ToolCall
	if hasTools {
		toolCalls = ExtractToolInvocationsWithFallback(fullContent.String(), fullReasoning.String())
		if len(toolCalls) > 0 {
			stopReason = "tool_calls"
			for i, tc := range toolCalls {
				toolChunk := ChatCompletionChunk{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   modelName,
					Choices: []Choice{{
						Index: 0,
						Delta: &Delta{
							ToolCalls: []ToolCall{{
								Index:    i,
								ID:       tc.ID,
								Type:     tc.Type,
								Function: tc.Function,
							}},
						},
						FinishReason: nil,
					}},
				}
				if !sendChunk(toolChunk) {
					return result
				}
			}
		} else {
			// 未检测到工具调用，将缓冲的 content 作为普通内容发送
			bufferedContent := RemoveToolJSONContent(fullContent.String())
			if bufferedContent != "" {
				chunk := ChatCompletionChunk{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   modelName,
					Choices: []Choice{{
						Index:        0,
						Delta:        &Delta{Content: bufferedContent},
						FinishReason: nil,
					}},
				}
				if !sendChunk(chunk) {
					return result
				}
			}
		}
	}

	if !hasContent {
		result.OutputTokens = outputTokens
		result.ErrorMessage = "empty response"
		return result
	}

	finalChunk := ChatCompletionChunk{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []Choice{{
			Index:        0,
			Delta:        &Delta{},
			FinishReason: &stopReason,
		}},
	}
	if !sendChunk(finalChunk) {
		return result
	}

	if includeUsage {
		usageChunk := ChatCompletionChunkResponse{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   modelName,
			Choices: []Choice{},
			Usage: &Usage{
				PromptTokens:     inputTokens,
				CompletionTokens: outputTokens,
				TotalTokens:      inputTokens + outputTokens,
			},
		}
		if !sendChunk(usageChunk) {
			return result
		}
	}

	if _, err := fmt.Fprintf(w, "data: [DONE]\n\n"); err != nil {
		result.Success = false
		result.ErrorMessage = err.Error()
		return result
	}
	flusher.Flush()

	result.HasContent = hasContent
	result.OutputTokens = outputTokens
	return result
}

// handleNonStreamResponseWithRetry 非流式响应处理（带重试支持，不立即写入响应）
func handleNonStreamResponseWithRetry(w http.ResponseWriter, body io.ReadCloser, completionID, modelName string, inputTokens int64, tools []Tool) UpstreamResult {
	result := UpstreamResult{Success: true, HasContent: false}
	var outputTokens int64
	var upstreamError string

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var chunks []string
	var reasoningChunks []string
	thinkingFilter := &ThinkingFilter{}
	searchRefFilter := NewSearchRefFilter()
	hasThinking := false
	diagHexDumped := false // 诊断用：只 dump 第一条 thinking 行的原始 hex
	pendingSourcesMarkdown := ""
	pendingImageSearchMarkdown := ""

	for scanner.Scan() {
		line := scanner.Text()
		LogDebug("[Upstream] %s", line)

		// 诊断：DEBUG 时，对第一条 thinking 行 dump 原始 hex，定位上游字节是否在到达
		// scanner 前就已损坏（区分是 tls-client/代理 还是 后续 json 解析的问题）。
		if Cfg.DebugLogging && !diagHexDumped && strings.Contains(line, `"phase":"thinking"`) {
			diagHexDumped = true
			hb := []byte(line)
			n := len(hb)
			if n > 400 {
				n = 400
			}
			LogDebug("[Diag][UpstreamRawHex] len=%d first%d=%x", len(line), n, hb[:n])
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var upstream UpstreamData
		if err := json.Unmarshal([]byte(payload), &upstream); err != nil {
			continue
		}

		// 检测上游错误
		if upstream.HasError() {
			upstreamError = upstream.GetErrorMessage()
			LogError("Upstream error: %s", upstreamError)
			result.Success = false
			result.ErrorMessage = upstreamError
			return result
		}

		if upstream.Data.Phase == "done" {
			break
		}

		if upstream.Data.Phase == "thinking" && upstream.Data.DeltaContent != "" {
			if thinkingFilter.lastPhase != "" && thinkingFilter.lastPhase != "thinking" {
				thinkingFilter.ResetForNewRound()
				thinkingFilter.thinkingRoundCount++
				if thinkingFilter.thinkingRoundCount > 1 {
					reasoningChunks = append(reasoningChunks, "\n\n")
				}
			}
			thinkingFilter.lastPhase = "thinking"

			hasThinking = true
			reasoningContent := cleanReasoningUTF8(thinkingFilter.ProcessThinking(upstream.Data.DeltaContent))
			if reasoningContent != "" {
				thinkingFilter.lastOutputChunk = reasoningContent
				reasoningChunks = append(reasoningChunks, reasoningContent)
			}
			continue
		}

		if upstream.Data.Phase != "" {
			thinkingFilter.lastPhase = upstream.Data.Phase
		}

		editContent := upstream.GetEditContent()
		if editContent != "" && IsSearchResultContent(editContent) {
			if results := ParseSearchResults(editContent); len(results) > 0 {
				searchRefFilter.AddSearchResults(results)
				pendingSourcesMarkdown = searchRefFilter.GetSearchResultsMarkdown()
			}
			continue
		}
		if editContent != "" && strings.Contains(editContent, `"search_image"`) {
			textBeforeBlock := ExtractTextBeforeGlmBlock(editContent)
			if textBeforeBlock != "" {
				chunks = append(chunks, textBeforeBlock)
			}
			if results := ParseImageSearchResults(editContent); len(results) > 0 {
				pendingImageSearchMarkdown = FormatImageSearchResults(results)
			}
			continue
		}
		if editContent != "" && strings.Contains(editContent, `"mcp"`) {
			textBeforeBlock := ExtractTextBeforeGlmBlock(editContent)
			if textBeforeBlock != "" {
				chunks = append(chunks, textBeforeBlock)
			}
			continue
		}
		if editContent != "" && IsSearchToolCall(editContent, upstream.Data.Phase) {
			continue
		}

		if pendingSourcesMarkdown != "" {
			if hasThinking {
				reasoningChunks = append(reasoningChunks, pendingSourcesMarkdown)
			} else {
				chunks = append(chunks, pendingSourcesMarkdown)
			}
			pendingSourcesMarkdown = ""
		}
		if pendingImageSearchMarkdown != "" {
			chunks = append(chunks, pendingImageSearchMarkdown)
			pendingImageSearchMarkdown = ""
		}

		content := ""
		if upstream.Data.Phase == "answer" && upstream.Data.DeltaContent != "" {
			content = upstream.Data.DeltaContent
		} else if upstream.Data.Phase == "answer" && editContent != "" {
			if strings.Contains(editContent, "</details>") {
				reasoningContent := cleanReasoningUTF8(thinkingFilter.ExtractIncrementalThinking(editContent))
				if reasoningContent != "" {
					reasoningChunks = append(reasoningChunks, reasoningContent)
				}

				if idx := strings.Index(editContent, "</details>"); idx != -1 {
					afterDetails := editContent[idx+len("</details>"):]
					if strings.HasPrefix(afterDetails, "\n") {
						content = afterDetails[1:]
					} else {
						content = afterDetails
					}
				}
			}
		} else if (upstream.Data.Phase == "other" || upstream.Data.Phase == "tool_call") && editContent != "" {
			content = editContent
		}

		if content != "" {
			chunks = append(chunks, content)
		}
	}

	fullContent := strings.Join(chunks, "")
	fullContent = searchRefFilter.Process(fullContent) + searchRefFilter.Flush()
	fullReasoning := strings.Join(reasoningChunks, "")
	fullReasoning = searchRefFilter.Process(fullReasoning) + searchRefFilter.Flush()

	// 检查是否有内容
	if fullContent == "" && fullReasoning == "" {
		result.HasContent = false
		result.ErrorMessage = "empty response"
		return result
	}

	result.HasContent = true

	// 检测工具调用
	stopReason := "stop"
	var toolCalls []ToolCall
	if len(tools) > 0 {
		toolCalls = ExtractToolInvocationsWithFallback(fullContent, fullReasoning)
		fullContent = RemoveToolJSONContent(fullContent)
		if len(toolCalls) > 0 {
			stopReason = "tool_calls"
		}
	}

	// 计算输出 token
	outputTokens = CountTokens(fullContent) + CountTokens(fullReasoning)
	result.OutputTokens = outputTokens

	// 写入响应
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", completionID)

	response := ChatCompletionResponse{
		ID:      completionID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []Choice{{
			Index: 0,
			Message: &MessageResp{
				Role:             "assistant",
				Content:          fullContent,
				ReasoningContent: fullReasoning,
				ToolCalls:        toolCalls,
			},
			FinishReason: &stopReason,
		}},
		Usage: &Usage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		},
		SystemFingerprint: "openai",
	}
	json.NewEncoder(w).Encode(response)
	return result
}

func HandleModels(w http.ResponseWriter, r *http.Request) {
	models := GetAvailableModels()

	response := ModelsResponse{
		Object: "list",
		Data:   models,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
