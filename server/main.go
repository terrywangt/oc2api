package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	cryptorand "crypto/rand"

	"gopkg.in/yaml.v3"
)

const (
	ProxyVersion = "v1.6.1"
	// OCVersion 是 UA 中上报的 OpenCode CLI 版本。
	// 2026-09-17 Zen 给免费层推理端点加了客户端校验(实测, 直连 https://opencode.ai/zen/v1/chat/completions):
	//   1) UA 必须形如 opencode/<semver> 且版本 >= 1.17.0 —— 低于则 426 UpgradeRequired
	//      ("OpenCode 1.17.0 or newer is required to use the free tier"),
	//      非 opencode UA 则 403 FreeTierError ("can only be used from within OpenCode")
	//   2) x-opencode-session 必须严格匹配 ses_[0-9a-f]{26}(26 位小写十六进制), 否则同样 403
	// 旧值 1.15.13 低于门槛, 且 session 是自定义长 ID —— 两个条件都不满足, 全部请求 403。
	// 上游抬高门槛时可用环境变量 OC_VERSION 覆盖, 无需改代码重新构建。
	OCVersion          = "1.18.31"
	ZenBaseURL         = "https://opencode.ai"
	ZenURL             = ZenBaseURL + "/zen/v1/chat/completions"
	ZenModelsURL       = ZenBaseURL + "/zen/v1/models"
	defaultTimeout     = 5 * time.Minute
	ImageFallbackModel = "mimo-v2.5-free" // DeepSeek 不支持图片,带图请求路由到该带图模型

	// 图片生成上游:免费、无需 key。OpenCode Zen 免费模型全部只输出文本(text-only),无生图能力,
	// 因此 /v1/images/generations 转发到 Pollinations 免费图片服务。
	PollinationsImageURL = "https://image.pollinations.ai/prompt/"
	PollinationsUA       = "Mozilla/5.0 (compatible; oc2api-image/1.0)"
)

// imageCacheDir 存放本地暂存的生成图片(response_format=url 时供 GET /images/{id} 取用)
const imageCacheDir = "/tmp/oc2api-images"

type Config struct {
	Port      int    `yaml:"port"`
	APIKey    string `yaml:"api-key"`
	Debug     bool   `yaml:"debug"`
	TimeoutMs int    `yaml:"timeout-ms"`
}

var (
	Cfg            *Config
	UserSessions   = &sync.Map{}
	CachedModels   []map[string]interface{}
	CachedModelsMu sync.RWMutex
	zenHTTPClient  = newZenHTTPClient()
)

var CORSHeaders = map[string]string{
	"Access-Control-Allow-Origin":  "*",
	"Access-Control-Allow-Methods": "GET, POST, OPTIONS",
	"Access-Control-Allow-Headers": "Authorization, X-API-Key, x-api-key, Content-Type, Anthropic-Version, Anthropic-Beta",
}

var JSONRespHeaders = map[string]string{
	"Content-Type": "application/json; charset=utf-8",
}

var SSERespHeaders = map[string]string{
	"Content-Type":      "text/event-stream; charset=utf-8",
	"Cache-Control":     "no-cache, no-transform",
	"X-Accel-Buffering": "no",
}

type thinkState struct {
	inThink        bool
	emittedContent bool
	removedThink   bool
}

type zenError struct {
	Message string
	Type    string
}

var thinkBlockRe = regexp.MustCompile(`(?is)<think>([\s\S]*?)<\/think>`)

func newThinkState() *thinkState {
	return &thinkState{}
}

func stripThinkStreamText(state *thinkState, text string) string {
	var output strings.Builder
	cursor := 0
	lower := strings.ToLower(text)

	for cursor < len(text) {
		if state.inThink {
			end := strings.Index(lower[cursor:], "</think>")
			if end == -1 {
				break
			}
			cursor += end + len("</think>")
			state.inThink = false
			state.removedThink = true
			continue
		}

		start := strings.Index(lower[cursor:], "<think>")
		if start == -1 {
			output.WriteString(text[cursor:])
			break
		}

		output.WriteString(text[cursor : cursor+start])
		cursor += start + len("<think>")
		state.inThink = true
		state.removedThink = true
	}

	if state.removedThink && !state.emittedContent && output.Len() > 0 {
		trimmed := strings.TrimLeft(output.String(), " ")
		output.Reset()
		output.WriteString(trimmed)
	}
	if output.Len() > 0 {
		state.emittedContent = true
	}
	return output.String()
}

func extractThinkBlocks(text string) string {
	matches := thinkBlockRe.FindAllStringSubmatch(text, -1)
	var parts []string
	for _, m := range matches {
		content := strings.TrimSpace(m[1])
		if content != "" {
			parts = append(parts, content)
		}
	}
	return strings.Join(parts, "\n")
}

func stripThinkBlocks(text string) string {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "think") {
		return text
	}
	state := newThinkState()
	return stripThinkStreamText(state, text)
}

func getThinkState(states map[int]*thinkState, key interface{}) *thinkState {
	idx := 0
	switch v := key.(type) {
	case float64:
		idx = int(v)
	case int:
		idx = v
	}
	if _, ok := states[idx]; !ok {
		states[idx] = newThinkState()
	}
	return states[idx]
}

// ocVersion 返回 UA 里使用的 OpenCode 版本, 允许 OC_VERSION 环境变量覆盖
// (Zen 后续抬高最低版本要求时, 重建容器带上 OC_VERSION 即可)。
func ocVersion() string {
	if v := strings.TrimSpace(os.Getenv("OC_VERSION")); v != "" {
		return v
	}
	return OCVersion
}

// zenUserAgent 构造 Zen 网关接受的 UA: opencode/<semver>(其余部分为真实 CLI 同款运行时标记)。
func zenUserAgent() string {
	return fmt.Sprintf("opencode/%s ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13", ocVersion())
}

// zenSessionID 生成 Zen 免费层要求的会话 ID: ses_ + 26 位小写十六进制。
// 实测 ses_+24 位、ses_+28 位、不带前缀的 hex 全部 403。
func zenSessionID() string {
	b := make([]byte, 13)
	cryptorand.Read(b)
	return "ses_" + hex.EncodeToString(b)
}

func ocId(prefix string) string {
	bytes := make([]byte, 12)
	cryptorand.Read(bytes)
	encoded := base64.RawURLEncoding.EncodeToString(bytes)
	return fmt.Sprintf("%s_%x%s", prefix, time.Now().UnixNano(), encoded)
}

type session struct {
	id string
	ts int64
}

func getSession(user string) string {
	now := time.Now().UnixMilli()
	if val, ok := UserSessions.Load(user); ok {
		s := val.(*session)
		if now-s.ts < 30*60*1000 {
			s.ts = now
			return s.id
		}
	}
	s := &session{id: zenSessionID(), ts: now}
	UserSessions.Store(user, s)
	return s.id
}

const sessionTTL = 30 * time.Minute

func sessionCleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now().UnixMilli()
		UserSessions.Range(func(key, val any) bool {
			s := val.(*session)
			if now-s.ts > sessionTTL.Milliseconds() {
				UserSessions.Delete(key)
			}
			return true
		})
	}
}

func authenticate(r *http.Request) (string, *apiError) {
	if Cfg.APIKey == "" {
		return "anonymous", nil
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		authHeader = r.Header.Get("X-Api-Key")
	}
	if authHeader == "" {
		authHeader = r.Header.Get("x-api-key")
	}
	if authHeader == "" {
		authHeader = r.Header.Get("Proxy-Authorization")
	}

	// 兼容代理客户端 Basic 认证（curl -x -U user:pass），密码等于 API_KEY 即通过
	if len(authHeader) > 6 && strings.EqualFold(authHeader[:6], "basic ") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(authHeader[6:]))
		if err == nil {
			s := string(decoded)
			pass := s
			if idx := strings.Index(s, ":"); idx >= 0 {
				pass = s[idx+1:]
			}
			if pass == Cfg.APIKey {
				return "user-proxy", nil
			}
		}
		return "", &apiError{
			message: "Invalid API key",
			errType: "authentication_error",
			status:  http.StatusUnauthorized,
		}
	}

	token := authHeader
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		token = strings.TrimSpace(authHeader[7:])
	}

	if token == Cfg.APIKey {
		return "user-default", nil
	}

	return "", &apiError{
		message: "Invalid API key",
		errType: "authentication_error",
		status:  http.StatusUnauthorized,
	}
}

func debugLog(label string, payload interface{}) {
	if !Cfg.Debug {
		return
	}
	data, _ := json.Marshal(payload)
	log.Println(label, string(data))
}

func byteLength(s string) int {
	return len([]byte(s))
}

func shortId(id string) string {
	if len(id) <= 16 {
		return id
	}
	return id[:8] + "..." + id[len(id)-6:]
}

func previewText(text string, max int) string {
	if max == 0 {
		max = 800
	}
	s := strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if len(s) > max {
		s = s[:max]
	}
	return s
}

func marshalJSON(v interface{}) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func deepCopy(v interface{}) interface{} {
	b, _ := json.Marshal(v)
	var out interface{}
	json.Unmarshal(b, &out)
	return out
}

func safeUnmarshal(text string) map[string]interface{} {
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return nil
	}
	return result
}

func getFloat(v interface{}, def float64) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case json.Number:
		f, _ := val.Float64()
		return f
	}
	return def
}

func getInt(v interface{}, def int) int {
	return int(getFloat(v, float64(def)))
}

func getString(v interface{}, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func JSONResponse(w http.ResponseWriter, data interface{}, status int) {
	b, _ := json.Marshal(data)
	for k, v := range CORSHeaders {
		w.Header().Set(k, v)
	}
	for k, v := range JSONRespHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(b)
}

func SetCORSHeaders(w http.ResponseWriter) {
	for k, v := range CORSHeaders {
		w.Header().Set(k, v)
	}
}

func SetSSEHeaders(w http.ResponseWriter) {
	for k, v := range SSERespHeaders {
		w.Header().Set(k, v)
	}
	for k, v := range CORSHeaders {
		w.Header().Set(k, v)
	}
}

type apiError struct {
	message string
	errType string
	status  int
	code    string
}

func makeAPIError(message, errType string, status int) *apiError {
	return &apiError{message: message, errType: errType, status: status}
}

func writeAPIError(w http.ResponseWriter, err *apiError) {
	writeOpenAIError(w, err.message, err.errType, err.status, err.code)
}

func writeOpenAIError(w http.ResponseWriter, message, errType string, status int, code string) {
	data := map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
		},
	}
	if code != "" {
		(data["error"].(map[string]interface{}))["code"] = code
	}
	JSONResponse(w, data, status)
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	msg := err.Error()
	errType := "upstream_error"
	status := http.StatusBadGateway
	prefix := "Upstream error: "
	if msg == "timeout" || msg == "context deadline exceeded" ||
		strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "Timeout exceeded") {
		msg = "Upstream timeout"
		errType = "timeout_error"
		status = http.StatusGatewayTimeout
		prefix = ""
	}
	writeOpenAIError(w, prefix+msg, errType, status, "")
}

func fetchZenModels() ([]map[string]interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ResolveTimeout(Cfg))
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", ZenModelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("User-Agent", zenUserAgent())

	started := time.Now()
	resp, err := zenHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	debugLog("[MODEL LIST]", map[string]interface{}{
		"status": resp.StatusCode,
		"ms":     int(time.Since(started).Milliseconds()),
	})

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Model list returned HTTP %d", resp.StatusCode)
	}

	parsed := safeUnmarshal(string(raw))
	if parsed == nil {
		return nil, fmt.Errorf("Invalid model list response")
	}

	data, ok := parsed["data"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("Invalid model list response")
	}

	var models []map[string]interface{}
	for _, item := range data {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if id, ok := m["id"].(string); ok && isAllowedModelId(id) {
			models = append(models, m)
		}
	}

	if len(models) == 0 {
		return nil, fmt.Errorf("No allowed models returned from upstream")
	}

	return models, nil
}

func isAllowedModelId(id string) bool {
	return id == "big-pickle" || strings.HasSuffix(id, "-free")
}

func getAvailableModels() ([]map[string]interface{}, error) {
	CachedModelsMu.RLock()
	if CachedModels != nil {
		defer CachedModelsMu.RUnlock()
		return CachedModels, nil
	}
	CachedModelsMu.RUnlock()

	CachedModelsMu.Lock()
	defer CachedModelsMu.Unlock()

	if CachedModels != nil {
		return CachedModels, nil
	}

	models, err := fetchZenModels()
	if err != nil {
		return nil, err
	}
	CachedModels = models
	return models, nil
}

type parsedBody struct {
	Body     map[string]interface{}
	ParseErr *apiError
}

func readRequestBody(r *http.Request) *parsedBody {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return &parsedBody{
			ParseErr: makeAPIError("Invalid JSON body", "invalid_request_error", http.StatusBadRequest),
		}
	}

	var body map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return &parsedBody{
			ParseErr: makeAPIError("Invalid JSON body", "invalid_request_error", http.StatusBadRequest),
		}
	}

	return &parsedBody{Body: body}
}

type zenRequest struct {
	Body    string
	Headers map[string]string
}

const reasoningPlaceholder = " "

func injectReasoningContent(model string, messages []interface{}) []interface{} {
	if len(messages) == 0 {
		return messages
	}

	var changed bool
	for _, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if getString(m["role"], "") != "assistant" {
			continue
		}
		if getString(m["reasoning_content"], "") != "" {
			continue
		}
		if deepSeekRegex.MatchString(model) {
			changed = true
			break
		}
	}

	if !changed {
		return messages
	}

	next := deepCopy(messages).([]interface{})
	for _, msg := range next {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if getString(m["role"], "") != "assistant" {
			continue
		}
		if getString(m["reasoning_content"], "") != "" {
			continue
		}
		if deepSeekRegex.MatchString(model) {
			m["reasoning_content"] = reasoningPlaceholder
		}
	}
	return next
}

var deepSeekRegex = regexp.MustCompile(`(?i)deepseek`)

func partIsImage(p map[string]interface{}) bool {
	t := getString(p["type"], "")
	return t == "image_url" || t == "image"
}

// DeepSeek 是否需要图片回退:仅当最近一条 user 消息带图时路由到 mimo。
// 历史残留的图片不触发,避免后续纯文字追问一直走 mimo。
func lastUserMessageHasImage(messages []interface{}) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		m, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		if getString(m["role"], "") != "user" {
			continue
		}
		content, ok := m["content"].([]interface{})
		if !ok {
			return false
		}
		for _, part := range content {
			p, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			if partIsImage(p) {
				return true
			}
		}
		return false
	}
	return false
}

func deepSeekNeedsImageFallback(model string, messages []interface{}) bool {
	return deepSeekRegex.MatchString(model) && lastUserMessageHasImage(messages)
}

// DeepSeek 上游对整段 messages 里任何一处的 image_url 都会反序列化报错。
// 纯文字追问会带上前一轮的图片消息,发给 DeepSeek 前把历史里的图片剥离掉。
func stripImagesForDeepSeek(messages []interface{}) []interface{} {
	if len(messages) == 0 {
		return messages
	}

	hasImage := false
	for _, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := m["content"].([]interface{})
		if !ok {
			continue
		}
		for _, part := range content {
			p, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			if partIsImage(p) {
				hasImage = true
				break
			}
		}
		if hasImage {
			break
		}
	}
	if !hasImage {
		return messages
	}

	next := make([]interface{}, len(messages))
	for i, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			next[i] = msg
			continue
		}
		content, ok := m["content"].([]interface{})
		if !ok {
			next[i] = m
			continue
		}

		var parts []interface{}
		for _, part := range content {
			p, ok := part.(map[string]interface{})
			if !ok {
				parts = append(parts, part)
				continue
			}
			if !partIsImage(p) {
				parts = append(parts, p)
			}
		}
		if len(parts) == len(content) {
			next[i] = m
			continue
		}

		copied := deepCopy(m).(map[string]interface{})
		switch {
		case len(parts) == 0:
			copied["content"] = "[图片]"
		case allTextParts(parts):
			var b strings.Builder
			for _, part := range parts {
				p, _ := part.(map[string]interface{})
				t, _ := p["text"].(string)
				b.WriteString(t)
			}
			copied["content"] = b.String()
		default:
			copied["content"] = parts
		}
		next[i] = copied
	}
	return next
}

func allTextParts(parts []interface{}) bool {
	for _, part := range parts {
		p, ok := part.(map[string]interface{})
		if !ok {
			return false
		}
		if getString(p["type"], "") != "text" {
			return false
		}
	}
	return true
}

func buildZenRequest(model string, messages, tools []interface{}, toolChoice interface{}, reasoningEffort, sessionId string, stream bool) *zenRequest {
	body := map[string]interface{}{
		"model":    model,
		"messages": messages,
		"stream":   stream,
	}
	// 按实际发送的模型判断:路由到图片模型时跳过 DS 专属的 reasoning_effort
	if deepSeekRegex.MatchString(model) {
		if reasoningEffort != "high" && reasoningEffort != "max" {
			reasoningEffort = "high"
		}
		body["reasoning_effort"] = reasoningEffort
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if toolChoice != nil {
		body["tool_choice"] = toolChoice
	}

	bodyBytes, _ := json.Marshal(body)

	headers := map[string]string{
		"Content-Type":       "application/json",
		"Authorization":      "Bearer public",
		"User-Agent":         zenUserAgent(),
		"x-opencode-client":  "desktop",
		"x-opencode-project": "global",
		"x-opencode-request": ocId("msg"),
		"x-opencode-session": sessionId,
	}
	if stream {
		headers["Accept"] = "text/event-stream"
	}

	return &zenRequest{
		Body:    string(bodyBytes),
		Headers: headers,
	}
}

func fetchZen(ctx context.Context, zenReq *zenRequest) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", ZenURL, strings.NewReader(zenReq.Body))
	if err != nil {
		return nil, err
	}
	for k, v := range zenReq.Headers {
		req.Header.Set(k, v)
	}
	client := zenHTTPClient
	if client == nil {
		client = newZenHTTPClient()
	}
	return client.Do(req)
}

func parseZenError(raw string) *zenError {
	text := strings.TrimSpace(raw)
	if !strings.HasPrefix(text, "{") {
		return nil
	}
	if !strings.Contains(text, "FreeUsageLimitError") &&
		!strings.Contains(text, `"error"`) &&
		!strings.Contains(text, `"type"`) {
		return nil
	}

	parsed := safeUnmarshal(text)
	if parsed == nil {
		return nil
	}

	errMap, _ := parsed["error"].(map[string]interface{})
	if errMap == nil && parsed["type"] != "error" {
		return nil
	}

	message := "Rate limit exceeded"
	if errMap != nil {
		if msg, ok := errMap["message"].(string); ok {
			message = msg
		}
	}
	if msg, ok := parsed["message"].(string); ok {
		message = msg
	}

	errType := "upstream_error"
	if errMap != nil {
		if et, ok := errMap["type"].(string); ok {
			errType = et
		}
	}
	if et, ok := parsed["type"].(string); ok {
		errType = et
	}

	return &zenError{Message: message, Type: errType}
}

func logZenRequest(requestId, format, model string, stream bool, user string, zenReq *zenRequest, messageCount int) {
	debugLog("[ZEN REQ]", map[string]interface{}{
		"requestId":    requestId,
		"format":       format,
		"user":         user,
		"model":        model,
		"stream":       stream,
		"messageCount": messageCount,
		"bodyBytes":    byteLength(zenReq.Body),
		"ocRequest":    shortId(zenReq.Headers["x-opencode-request"]),
		"ocSession":    shortId(zenReq.Headers["x-opencode-session"]),
	})
}

func logZenResponse(env string, payload map[string]interface{}) {
	if !Cfg.Debug {
		status := getFloat(payload["status"], 0)
		if int(status) < 400 {
			return
		}
	}
	log.Println("[ZEN RES]", marshalJSON(payload))
}

func logUpstreamBody(env, requestId, model string, status int, raw string, zenErr *zenError, firstChunk bool) {
	body := raw
	shouldLog := Cfg.Debug || status >= 400 || zenErr != nil
	if !shouldLog {
		return
	}

	shouldPrintPreview := zenErr != nil || status >= 400 || Cfg.Debug
	payload := map[string]interface{}{
		"requestId":  requestId,
		"model":      model,
		"status":     status,
		"firstChunk": firstChunk,
		"chars":      len(body),
	}
	if zenErr != nil {
		payload["zenError"] = map[string]interface{}{
			"message": zenErr.Message,
			"type":    zenErr.Type,
		}
	}
	if shouldPrintPreview {
		payload["preview"] = previewText(body, 800)
	}

	log.Println("[ZEN BODY]", marshalJSON(payload))
}

func normalizeOpenAIFullData(data map[string]interface{}, model string) map[string]interface{} {
	next := deepCopy(data).(map[string]interface{})
	if model != "" {
		next["model"] = model
	}
	choices, ok := next["choices"].([]interface{})
	if !ok {
		return next
	}

	var newChoices []interface{}
	for _, c := range choices {
		choice, ok := c.(map[string]interface{})
		if !ok {
			newChoices = append(newChoices, c)
			continue
		}
		if choice["message"] == nil {
			newChoices = append(newChoices, choice)
			continue
		}

		message := deepCopy(choice["message"]).(map[string]interface{})
		normalizeReasoningField(message)

		if content, ok := message["content"].(string); ok {
			reasoning := extractThinkBlocks(content)
			if reasoning != "" && message["reasoning_content"] == nil {
				message["reasoning_content"] = reasoning
			}
			visible := stripThinkBlocks(content)
			if visible != content {
				message["content"] = visible
			}
		}

		newChoice := deepCopy(choice).(map[string]interface{})
		newChoice["message"] = message
		newChoices = append(newChoices, newChoice)
	}

	next["choices"] = newChoices
	return next
}

type streamNormalizer struct {
	contentStates map[int]*thinkState
	model         string
}

func newStreamNormalizer(model string) *streamNormalizer {
	return &streamNormalizer{contentStates: make(map[int]*thinkState), model: model}
}

func (n *streamNormalizer) normalize(chunk map[string]interface{}) map[string]interface{} {
	if chunk == nil {
		return nil
	}

	choices, ok := chunk["choices"].([]interface{})
	if !ok {
		return nil
	}

	if len(choices) == 0 && chunk["cost"] != nil {
		return nil
	}

	next := deepCopy(chunk).(map[string]interface{})
	delete(next, "cost")
	if n.model != "" {
		next["model"] = n.model
	}

	var newChoices []interface{}
	for _, c := range choices {
		choice, ok := c.(map[string]interface{})
		if !ok {
			newChoices = append(newChoices, c)
			continue
		}
		normalized := normalizeStreamChoice(choice, n.contentStates)
		if normalized != nil {
			newChoices = append(newChoices, normalized)
		}
	}

	next["choices"] = newChoices
	if len(newChoices) == 0 && next["usage"] == nil {
		return nil
	}
	return next
}

func normalizeStreamChoice(choice map[string]interface{}, states map[int]*thinkState) map[string]interface{} {
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return choice
	}

	deltaCopy := deepCopy(delta).(map[string]interface{})
	normalizeReasoningField(deltaCopy)

	if content, ok := deltaCopy["content"].(string); ok {
		state := getThinkState(states, choice["index"])
		visible := stripThinkStreamText(state, content)
		if visible != "" {
			deltaCopy["content"] = visible
		} else {
			delete(deltaCopy, "content")
		}
	}

	if len(deltaCopy) == 0 && getString(choice["finish_reason"], "") == "" {
		return nil
	}

	result := deepCopy(choice).(map[string]interface{})
	result["delta"] = deltaCopy
	return result
}

func normalizeReasoningField(target map[string]interface{}) {
	if target == nil {
		return
	}
	reasoning, ok := target["reasoning"].(string)
	if ok && reasoning != "" && target["reasoning_content"] == nil {
		target["reasoning_content"] = reasoning
	}
	delete(target, "reasoning")
}

func HandleOpenAI(w http.ResponseWriter, r *http.Request, env string) {
	requestId := ocId("req")
	user, authErr := authenticate(r)
	if authErr != nil {
		writeAPIError(w, authErr)
		return
	}

	input := readRequestBody(r)
	if input.ParseErr != nil {
		writeAPIError(w, input.ParseErr)
		return
	}

	model := getString(input.Body["model"], "")
	messages, _ := input.Body["messages"].([]interface{})
	stream := false
	if s, ok := input.Body["stream"].(bool); ok {
		stream = s
	}
	tools, _ := input.Body["tools"].([]interface{})
	toolChoice := input.Body["tool_choice"]
	reasoningEffort := getString(input.Body["reasoning_effort"], "")
	if reasoningEffort == "" {
		reasoningEffort = getString(input.Body["reasoningEffort"], "")
	}

	sessionId := getSession(user)

	useImageModel := deepSeekNeedsImageFallback(model, messages)
	upstreamModel := model
	if useImageModel {
		upstreamModel = ImageFallbackModel
	}
	// 仅当真正发给 DeepSeek 时才剥离历史图片(DeepSeek 无法解析 image_url)。
	// 注意:修复前条件是 upstreamModel == model,对 mimo 等带图模型直接请求时
	// 也会误剥图片,导致图片永远到不了 Zen。正确条件是判断最终上游模型。
	transformedMessages := messages
	if deepSeekRegex.MatchString(upstreamModel) {
		transformedMessages = stripImagesForDeepSeek(messages)
	}
	transformedMessages = injectReasoningContent(upstreamModel, transformedMessages)

	msgSummary := formatMsgSummary(transformedMessages)
	log.Println("[OAI]", time.Now().UTC().Format(time.RFC3339), user, model,
		map[bool]string{true: "stream", false: "sync"}[stream], "msgs:", msgSummary)

	zenReq := buildZenRequest(upstreamModel, transformedMessages, tools, toolChoice, reasoningEffort, sessionId, stream)
	logZenRequest(requestId, "openai", model, stream, user, zenReq, len(messages))

	upstream, err := fetchZen(r.Context(), zenReq)
	if err != nil {
		debugLog("[ZEN FETCH ERROR]", map[string]interface{}{
			"requestId": requestId, "model": upstreamModel, "stream": stream,
			"message": err.Error(),
		})
		writeUpstreamError(w, err)
		return
	}
	defer upstream.Body.Close()

	logZenResponse(env, map[string]interface{}{
		"requestId": requestId, "model": upstreamModel, "stream": stream,
		"status": upstream.StatusCode, "ok": upstream.StatusCode < 400,
		"ms": 0,
	})

	if stream {
		OpenAIStreamResponse(w, r, upstream, requestId, model, env)
		return
	}
	OpenAIFullResponse(w, upstream, requestId, model, env)
}

func OpenAIFullResponse(w http.ResponseWriter, upstream *http.Response, requestId, model, env string) {
	raw, err := io.ReadAll(upstream.Body)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	data := safeUnmarshal(string(raw))
	zenErr := parseZenError(string(raw))
	logUpstreamBody(env, requestId, model, upstream.StatusCode, string(raw), zenErr, false)

	if upstream.StatusCode == http.StatusTooManyRequests || zenErr != nil {
		msg := "Rate limit exceeded"
		if zenErr != nil {
			msg = zenErr.Message
		}
		writeOpenAIError(w, msg+" (free model rate limit)", "rate_limit_error",
			http.StatusTooManyRequests, "rate_limit_exceeded")
		return
	}

	if data != nil && data["choices"] != nil {
		JSONResponse(w, normalizeOpenAIFullData(data, model), upstream.StatusCode)
		return
	}

	contentType := upstream.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	for k, v := range CORSHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(upstream.StatusCode)
	w.Write(raw)
}

func OpenAIStreamResponse(w http.ResponseWriter, r *http.Request, upstream *http.Response, requestId, model, env string) {
	if upstream.Body == nil {
		writeOpenAIError(w, "Empty response from upstream", "upstream_error", http.StatusBadGateway, "")
		return
	}

	peeker := bufio.NewReaderSize(upstream.Body, 64*1024)
	firstText, err := peeker.ReadString('\n') // 跨大行不截断
	if err != nil && firstText == "" {        // EOF 且无任何字节 → 空 body
		writeOpenAIError(w, "Empty response from upstream", "upstream_error", http.StatusBadGateway, "")
		return
	}
	firstText = strings.TrimRight(firstText, "\n")

	zenErr := parseZenError(firstText)
	if upstream.StatusCode == http.StatusTooManyRequests || zenErr != nil {
		logUpstreamBody(env, requestId, model, upstream.StatusCode, firstText, zenErr, true)
		msg := "Rate limit exceeded"
		if zenErr != nil {
			msg = zenErr.Message
		}
		writeOpenAIError(w, msg+" (free model rate limit)", "rate_limit_error",
			http.StatusTooManyRequests, "rate_limit_exceeded")
		return
	}

	SetSSEHeaders(w)
	w.WriteHeader(upstream.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	normalizer := newStreamNormalizer(model)
	doneSent := false

	sendSSE := func(data interface{}) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		flusher.Flush()
	}

	sendDone := func() {
		if doneSent {
			return
		}
		doneSent = true
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	processLine := func(line string) {
		if !strings.HasPrefix(line, "data:") {
			return
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			if payload == "[DONE]" {
				sendDone()
			}
			return
		}

		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			return
		}

		normalized := normalizer.normalize(parsed)
		if normalized != nil {
			sendSSE(normalized)
		}
	}

	if firstText != "" {
		processLine(firstText)
	}

	scanner := bufio.NewScanner(peeker)
	buffer := make([]byte, 0, 64*1024)
	scanner.Buffer(buffer, 64*1024*1024)
	for scanner.Scan() {
		processLine(scanner.Text())
		if r.Context().Err() != nil {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		log.Println("[SSE SCAN ERROR]", err)
	}

	sendDone()
}

func formatMsgSummary(messages []interface{}) string {
	var parts []string
	for _, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		role := getString(m["role"], "?")
		var length int
		if content, ok := m["content"].(string); ok {
			length = len(content)
		} else {
			length = len(marshalJSON(m["content"]))
		}
		parts = append(parts, fmt.Sprintf("%s:%d", role, length))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func Handler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		SetCORSHeaders(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	SetCORSHeaders(w)

	// 标准 HTTP 代理 absolute-form（curl -x 场景）：请求行是完整 URL 且目标不是本域
	if r.URL.IsAbs() {
		host := requestHost(r)
		if host == "" || !strings.EqualFold(r.URL.Hostname(), host) {
			ProxyResponse(w, r, r.URL.String())
			return
		}
	}

	path := strings.TrimRight(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}

	// 全局鉴权门：当 API_KEY 已设置（不为空）时，对所有路由进行密钥检查。
	// 这样 /、/health、/ip、/proxy 等辅助接口也会被保护，避免泄露内部信息。
	publicImagePath := r.Method == "GET" && strings.HasPrefix(path, "/images/")
	if Cfg.APIKey != "" && !publicImagePath {
		if _, authErr := authenticate(r); authErr != nil {
			writeAPIError(w, authErr)
			return
		}
	}

	switch {
	case r.Method == "GET" && path == "/":
		HealthResponse(w)
	case r.Method == "GET" && path == "/health":
		HealthResponse(w)
	case r.Method == "GET" && path == "/ip":
		IPResponse(w, r)
	case r.Method == "GET" && (path == "/v1/models" || path == "/models"):
		ModelsResponse(w, r)
	case r.Method == "POST" && (path == "/v1/chat/completions" || path == "/chat/completions"):
		HandleOpenAI(w, r, "")
	case path == "/proxy" || strings.HasPrefix(path, "/proxy/"):
		target := r.URL.Query().Get("url")
		if target == "" {
			// 用原始 RequestURI 提取，避免 Go URL 解析把路径中的 // 折叠（/proxy/https://x → https://x）
			raw := r.RequestURI
			if idx := strings.Index(raw, "?"); idx >= 0 {
				raw = raw[:idx]
			}
			raw = strings.TrimPrefix(raw, "/proxy")
			raw = strings.TrimPrefix(raw, "/")
			target = raw
		}
		if target == "" {
			JSONResponse(w, map[string]interface{}{"error": map[string]interface{}{"message": "Missing target. Use /proxy?url=<encoded-url>"}}, http.StatusBadRequest)
			return
		}
		ProxyResponse(w, r, target)
	case r.Method == "POST" && (path == "/v1/images/generations" || path == "/images/generations"):
		HandleImageGeneration(w, r)
	case r.Method == "GET" && strings.HasPrefix(path, "/images/"):
		serveCachedImage(w, r)
	default:
		JSONResponse(w, map[string]interface{}{"error": map[string]interface{}{"message": "Not found"}}, http.StatusNotFound)
	}
}

// requestHost 返回本服务对外的主机名（优先 X-Forwarded-Host，其次 Host），不带端口、小写
func requestHost(r *http.Request) string {
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if idx := strings.Index(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	return strings.ToLower(host)
}

// ---- 图片生成接口（/v1/images/generations）----
// OpenAI Images API 兼容格式。上游: Pollinations 免费图片服务(无需 key)。
// 支持参数:model, prompt, n, size, quality, response_format
// response_format: "b64_json"(默认) 或 "url"(返回本服务 GET /images/{id} 可取)

type imageGenRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              *int   `json:"n"`
	Size           string `json:"size"`
	Quality        string `json:"quality"`
	ResponseFormat string `json:"response_format"`
}

func parseImageSize(s string) (w, h int) {
	w, h = 512, 512
	if s == "" {
		return
	}
	parts := strings.SplitN(strings.ToLower(s), "x", 2)
	if len(parts) == 2 {
		if ww, err := strconv.Atoi(parts[0]); err == nil && ww > 0 {
			w = ww
		}
		if hh, err := strconv.Atoi(parts[1]); err == nil && hh > 0 {
			h = hh
		}
	}
	// Pollinations 上游:宽高非 16 倍数会被缩放,但无报错,这里透传
	return
}

func resolvePollinationsModel(reqModel string) string {
	// 支持的上游模型别名:flux(默认)、turbo、turbo(旧)
	switch strings.ToLower(reqModel) {
	case "turbo", "stable-diffusion", "sd", "sdxl":
		return "turbo"
	case "flux", "":
		return "flux"
	default:
		// 未知模型也走 flux(免费用)、不报错
		return "flux"
	}
}

func HandleImageGeneration(w http.ResponseWriter, r *http.Request) {
	if _, authErr := authenticate(r); authErr != nil {
		writeAPIError(w, authErr)
		return
	}

	var req imageGenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, makeAPIError("Invalid request body: "+err.Error(), "invalid_request_error", http.StatusBadRequest))
		return
	}
	if req.Prompt == "" {
		writeAPIError(w, makeAPIError("prompt is required", "invalid_request_error", http.StatusBadRequest))
		return
	}

	n := 1
	if req.N != nil && *req.N > 0 {
		n = *req.N
		if n > 10 {
			n = 10
		}
	}

	respFormat := "b64_json"
	if strings.EqualFold(req.ResponseFormat, "url") {
		respFormat = "url"
	}

	wImg, hImg := parseImageSize(req.Size)
	pollModel := resolvePollinationsModel(req.Model)

	created := time.Now().Unix()
	var data []map[string]interface{}

	for i := 0; i < n; i++ {
		seed := int(time.Now().UnixNano()%2147483647) + i
		promptEncoded := url.PathEscape(req.Prompt)
		genURL := fmt.Sprintf("%s%s?model=%s&width=%d&height=%d&seed=%d&nologo=true",
			PollinationsImageURL, promptEncoded, pollModel, wImg, hImg, seed)

		var imgData []byte
		for attempt := 0; attempt < 2; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			imgReq, err := http.NewRequestWithContext(ctx, "GET", genURL, nil)
			if err != nil {
				cancel()
				writeUpstreamError(w, err)
				return
			}
			imgReq.Header.Set("User-Agent", PollinationsUA)
			imgReq.Header.Set("Accept", "image/jpeg,image/png,*/*")

			imgResp, err := zenHTTPClient.Do(imgReq)
			if err != nil {
				cancel()
				writeUpstreamError(w, err)
				return
			}

			imgData, err = io.ReadAll(io.LimitReader(imgResp.Body, 20*1024*1024)) // 最大 20MB
			imgResp.Body.Close()
			cancel()

			if err != nil {
				writeUpstreamError(w, err)
				return
			}
			if imgResp.StatusCode == http.StatusOK {
				break
			}
			if attempt == 0 && imgResp.StatusCode >= 500 {
				debugLog("[IMAGE GEN RETRY]", map[string]interface{}{"seed": seed, "status": imgResp.StatusCode, "attempt": 1})
				seed = int(time.Now().UnixNano()%2147483647) + i // 重试用新 seed
				genURL = fmt.Sprintf("%s%s?model=%s&width=%d&height=%d&seed=%d&nologo=true",
					PollinationsImageURL, promptEncoded, pollModel, wImg, hImg, seed)
				continue
			}
			writeOpenAIError(w, fmt.Sprintf("Upstream image generation failed: HTTP %d", imgResp.StatusCode), "upstream_error", http.StatusBadGateway, "")
			return
		}
		if imgData == nil {
			writeOpenAIError(w, "Upstream image generation failed: empty response", "upstream_error", http.StatusBadGateway, "")
			return
		}

		if respFormat == "url" {
			// 写到本地暂存文件,返回本服务 URL
			imgId := fmt.Sprintf("%d_%d_%d", created, i, seed)
			imgPath := fmt.Sprintf("%s/%s.jpg", imageCacheDir, imgId)
			if err := os.WriteFile(imgPath, imgData, 0644); err != nil {
				writeUpstreamError(w, fmt.Errorf("write image cache: %w", err))
				return
			}
			// 获取本服务 host(对外可达的),拼接 URL
			host := requestHost(r)
			proto := "https"
			if r.TLS == nil {
				if fwdProto := r.Header.Get("X-Forwarded-Proto"); fwdProto != "" {
					proto = fwdProto
				}
			}
			imgURL := fmt.Sprintf("%s://%s/images/%s.jpg", proto, host, imgId)
			data = append(data, map[string]interface{}{"url": imgURL})
			debugLog("[IMAGE GEN]", map[string]interface{}{"index": i, "seed": seed, "size": len(imgData), "url": imgURL})
		} else {
			b64 := base64.StdEncoding.EncodeToString(imgData)
			data = append(data, map[string]interface{}{"b64_json": b64})
			debugLog("[IMAGE GEN]", map[string]interface{}{"index": i, "seed": seed, "size": len(imgData), "format": "b64_json"})
		}
	}

	JSONResponse(w, map[string]interface{}{
		"created": created,
		"data":    data,
	}, http.StatusOK)
}

// GET /images/{id}.jpg —— response_format=url 时返回本地暂存图片
func serveCachedImage(w http.ResponseWriter, r *http.Request) {
	// 路径: /images/{filename}
	name := strings.TrimPrefix(r.URL.Path, "/images/")
	name = strings.TrimPrefix(name, "/")
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		JSONResponse(w, map[string]interface{}{"error": map[string]interface{}{"message": "Not found"}}, http.StatusNotFound)
		return
	}
	imgPath := fmt.Sprintf("%s/%s", imageCacheDir, name)
	imgData, err := os.ReadFile(imgPath)
	if err != nil {
		JSONResponse(w, map[string]interface{}{"error": map[string]interface{}{"message": "Image not found or expired"}}, http.StatusNotFound)
		return
	}
	for k, v := range CORSHeaders {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	w.Write(imgData)
}

// ---- HTTP 代理接口（与 JS 版 /proxy 对齐）----
// 用法：
//   1. /proxy?url=<encoded-url>（任意方法，URL 在 query 参数）
//   2. /proxy/https://target/...（URL 直接跟在路径后）
//   3. 标准 HTTP 代理 absolute-form（curl -x http://<host> 直达）
// 认证与 AI 接口一致：Authorization: Bearer <API_KEY> / X-API-Key / Proxy-Authorization

// proxyBlockedReqHeaders：转发上游时剔除的请求头（防 hop-by-hop 头泄漏与代理递归）
var proxyBlockedReqHeaders = map[string]bool{
	"host": true, "content-length": true, "connection": true, "keep-alive": true,
	"te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
	"proxy-authorization": true, "proxy-connection": true, "accept-encoding": true,
	"cf-connecting-ip": true, "cf-ray": true, "cf-visitor": true, "cf-ipcountry": true,
	"x-vercel-id": true, "x-vercel-forwarded-for": true, "x-vercel-deployment-url": true,
	"x-forwarded-for": true, "x-forwarded-host": true, "x-forwarded-proto": true,
	"x-forwarded-port": true, "x-real-ip": true,
}

// proxyBlockedResHeaders：回传客户端时剔除的响应头（body 由 io.Copy 流式透传，长度由 Go 自动处理）
var proxyBlockedResHeaders = map[string]bool{
	"content-length": true, "transfer-encoding": true, "connection": true,
	"keep-alive": true, "content-encoding": true,
}

// isProxyTargetAllowed 校验目标 URL：仅 http/https，且拒绝内网/本机地址（SSRF 防护，与 JS 版一致）
func isProxyTargetAllowed(u *url.URL) bool {
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return false // 原始 IPv6 一律拒绝
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return false
		}
		return true // 公网 IP 放行
	}
	// 域名形式：字符串前缀匹配（与 JS 正则等价）
	if host == "localhost" ||
		strings.HasPrefix(host, "127.") ||
		strings.HasPrefix(host, "10.") ||
		strings.HasPrefix(host, "192.168.") ||
		strings.HasPrefix(host, "169.254.") {
		return false
	}
	if strings.HasPrefix(host, "172.") {
		parts := strings.Split(host, ".")
		if len(parts) >= 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil && n >= 16 && n <= 31 {
				return false
			}
		}
	}
	return true
}

// ProxyResponse 实现 /proxy 转发（认证、SSRF 校验、请求/响应头过滤、流式透传）
func ProxyResponse(w http.ResponseWriter, r *http.Request, target string) {
	if _, authErr := authenticate(r); authErr != nil {
		writeAPIError(w, authErr)
		return
	}

	u, err := url.Parse(target)
	if err != nil {
		JSONResponse(w, map[string]interface{}{"error": map[string]interface{}{"message": "Invalid target URL"}}, http.StatusBadRequest)
		return
	}
	if !isProxyTargetAllowed(u) {
		JSONResponse(w, map[string]interface{}{"error": map[string]interface{}{"message": "Target not allowed"}}, http.StatusForbidden)
		return
	}

	var body io.Reader
	if r.Method != "GET" && r.Method != "HEAD" {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), body)
	if err != nil {
		JSONResponse(w, map[string]interface{}{"error": map[string]interface{}{"message": "Invalid target URL"}}, http.StatusBadRequest)
		return
	}
	for k, vv := range r.Header {
		if proxyBlockedReqHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	client := &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return nil
		},
		Transport: &http.Transport{DisableCompression: true},
	}
	resp, err := client.Do(req)
	if err != nil {
		debugLog("[PROXY ERROR]", map[string]interface{}{"target": u.String(), "message": err.Error()})
		JSONResponse(w, map[string]interface{}{"error": map[string]interface{}{"message": "Proxy upstream error: " + err.Error()}}, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		if proxyBlockedResHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

var ipv4Regex = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)

var ipProviders = []string{
	"https://api.ipquery.io",
	"http://ip-api.com/json",
}

func IPResponse(w http.ResponseWriter, r *http.Request) {
	type result struct {
		ip     string
		source string
	}
	results := make(chan result, len(ipProviders))
	for _, url := range ipProviders {
		url := url
		go func() {
			results <- fetchIPFrom(url)
		}()
	}

	for range ipProviders {
		res := <-results
		if res.ip != "" {
			JSONResponse(w, map[string]interface{}{
				"ip":     res.ip,
				"source": res.source,
			}, http.StatusOK)
			return
		}
	}
	writeUpstreamError(w, fmt.Errorf("all IP providers failed"))
}

func fetchIPFrom(url string) (result struct {
	ip     string
	source string
}) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	if resp.StatusCode != http.StatusOK {
		return
	}

	if m := ipv4Regex.FindString(string(body)); m != "" && net.ParseIP(m) != nil && net.ParseIP(m).To4() != nil {
		result.ip = m
		result.source = url
	}
	return
}

func HealthResponse(w http.ResponseWriter) {
	JSONResponse(w, map[string]interface{}{
		"status":    "ok",
		"version":   ProxyVersion,
		"endpoints": []string{"/v1/chat/completions", "/chat/completions", "/v1/images/generations", "/images/generations", "/v1/models", "/models", "/proxy", "/health", "/ip"},
	}, http.StatusOK)
}

func ModelsResponse(w http.ResponseWriter, r *http.Request) {
	user, authErr := authenticate(r)
	if authErr != nil {
		writeAPIError(w, authErr)
		return
	}
	_ = user

	models, err := getAvailableModels()
	if err != nil {
		debugLog("[MODEL LIST ERROR]", map[string]interface{}{"message": err.Error()})
		writeUpstreamError(w, err)
		return
	}
	JSONResponse(w, map[string]interface{}{
		"object": "list",
		"data":   models,
	}, http.StatusOK)
}

func LoadConfig() *Config {
	cfg := &Config{
		Port: 8080,
	}

	data, err := os.ReadFile("config.yaml")
	if err != nil {
		log.Println("No config.yaml found, using defaults")
	} else if err := yaml.Unmarshal(data, cfg); err != nil {
		log.Println("Failed to parse config.yaml:", err)
	}

	// 环境变量覆盖（与 Vercel/Railway/Render README 对齐）。优先级高于 config.yaml。
	if v := os.Getenv("API_KEY"); v != "" {
		cfg.APIKey = v
		log.Println("API_KEY loaded from API_KEY environment variable")
	}
	if v := os.Getenv("DEBUG"); v != "" {
		cfg.Debug = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Port = p
		}
	}
	if v := os.Getenv("TIMEOUT_MS"); v != "" {
		if t, err := strconv.Atoi(v); err == nil {
			cfg.TimeoutMs = t
		}
	}

	return cfg
}

func ResolveTimeout(cfg *Config) time.Duration {
	if Cfg != nil && Cfg.TimeoutMs > 0 {
		return time.Duration(cfg.TimeoutMs) * time.Millisecond
	}
	return defaultTimeout
}

func newZenHTTPClient() *http.Client {
	// ResponseHeaderTimeout 覆盖「连接 + 等待响应头」，等价于 JS 的 FETCH_TIMEOUT_MS；
	// 不设整体 Timeout，避免长流被硬切（body 流式读取无时长上限）。
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: ResolveTimeout(Cfg)},
	}
}

func main() {
	log.SetOutput(os.Stdout)
	Cfg = LoadConfig()
	// 包变量区初始化时 Cfg 还是 nil，这里重建 client 让 timeout-ms 生效
	zenHTTPClient = newZenHTTPClient()

	go sessionCleanupLoop()

	// 不用 ServeMux：其 cleanPath 会把 /proxy/https://example.com 的 // 折叠成 301 重定向，
	// 破坏 /proxy/<完整URL> 路径形式。直接挂 HandlerFunc 保留原始 RequestURI。
	// 创建图片缓存目录(用于 response_format=url 时暂存生成的图片)
	os.MkdirAll(imageCacheDir, 0755)

	handler := http.HandlerFunc(Handler)

	port := fmt.Sprintf("%d", Cfg.Port)
	if Cfg.Port == 0 {
		port = "8080"
	}

	log.Println("OC2API server running on http://localhost:" + port)
	log.Println("Health check: http://localhost:" + port + "/health")
	log.Println("Version:", ProxyVersion)

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadTimeout:       ResolveTimeout(Cfg),
		WriteTimeout:      0, // 关闭写超时，SSE 长流不被切
		IdleTimeout:       ResolveTimeout(Cfg),
		ReadHeaderTimeout: ResolveTimeout(Cfg),
	}

	log.Fatal(server.ListenAndServe())
}
