const OC_VERSION = "1.18.31";
const PROXY_VERSION = "v1.6.1";

// 兼容多平台环境变量读取：Cloudflare Worker 通过 __OC_ENV__（env 参数）注入，
// Vercel / Node 走 process.env
const getEnv = (name) => globalThis.__OC_ENV__?.[name] ?? globalThis.process?.env?.[name];
const ZEN_BASE_URL = getEnv("ZEN_BASE_URL") || "https://opencode.ai";
const ZEN_URL = `${ZEN_BASE_URL}/zen/v1/chat/completions`;
const ZEN_MODELS_URL = `${ZEN_BASE_URL}/zen/v1/models`;
const FETCH_TIMEOUT_MS = 5 * 60 * 1000;
const IMAGE_FALLBACK_MODEL = "mimo-v2.5-free"; // DeepSeek 不支持图片,带图请求路由到该带图模型

// SHOW_REASONING: 是否将上游 reasoning_content 转发给客户端（默认 false = 省流量）
// REASONING_EFFORT: 覆盖上游推理强度（low / medium / high / max，默认 high）
const SHOW_REASONING = /^(true|1|yes)$/i.test(getEnv("SHOW_REASONING"));
const REASONING_EFFORT = getEnv("REASONING_EFFORT") || "high";

const userSessions = new Map();
let cachedModels = null;

const CORS_HEADERS = {
	"Access-Control-Allow-Origin": "*",
	"Access-Control-Allow-Methods": "GET, POST, OPTIONS",
	"Access-Control-Allow-Headers": "Authorization, X-API-Key, x-api-key, Content-Type, Anthropic-Version, Anthropic-Beta",
};

const JSON_HEADERS = {
	"Content-Type": "application/json; charset=utf-8",
};

const SSE_HEADERS = {
	"Content-Type": "text/event-stream; charset=utf-8",
	"Cache-Control": "no-cache, no-transform",
	"X-Accel-Buffering": "no",
};

export const config = {
	api: {
		bodyParser: false,
	},
};

export default async function handler(request, response) {
	const fetchRequest = isWebRequest(request)
		? request
		: await nodeRequestToFetchRequest(request);
	const fetchResponse = await handleRequest(fetchRequest);

	if (!response) return fetchResponse;
	return sendNodeResponse(response, fetchResponse);
}

async function handleRequest(request) {
	if (request.method === "OPTIONS") {
		return new Response(null, { status: 204, headers: CORS_HEADERS });
	}

	const url = new URL(request.url);
	const path = url.pathname.replace(/\/+$/, "") || "/";

	try {
		if (request.method === "GET" && (path === "/" || path === "/health")) return healthResponse();

		if (request.method === "GET" && path === "/ip") return ipResponse();
		// require auth for all other endpoints
		const auth = authenticate(request);
		if (auth.error) return auth.error;

		if (request.method === "GET" && (path === "/v1/models" || path === "/models")) return modelsResponse();
		if (request.method === "POST" && (path === "/v1/chat/completions" || path === "/chat/completions")) return handleOpenAI(request);

		// HTTP 代理接口（与 AI 接口同一套认证）
		if (path === "/proxy" || path.startsWith("/proxy/")) {
			const target =
				url.searchParams.get("url") ||
				(path.length > 7 && path.slice(1, 7) === "proxy" ? path.slice(7) : null);
			if (!target) return jsonResponse({ error: { message: "Missing target. Use /proxy?url=<encoded-url>" } }, 400);
			return handleProxy(request, target);
		}

		// 标准 HTTP 代理 absolute-form：GET http://target/... 直达本域名（curl -x 场景）
		const rawUrl = request.url;
		if ((rawUrl.startsWith("http://") || rawUrl.startsWith("https://")) && url.hostname !== requestHost(request)) {
			return handleProxy(request, rawUrl);
		}

		return jsonResponse({ error: { message: "Not found" } }, 404);
	} catch (error) {
		console.log("[FUNCTION ERROR]", error?.stack || error?.message || error);
		return jsonResponse({ error: { message: "Internal error", type: "server_error" } }, 500);
	}
}

function isWebRequest(request) {
	return typeof request?.headers?.get === "function" && typeof request?.arrayBuffer === "function";
}

async function nodeRequestToFetchRequest(request) {
	const headers = new Headers();
	for (const [key, value] of Object.entries(request.headers || {})) {
		if (Array.isArray(value)) {
			for (const item of value) headers.append(key, item);
		} else if (value !== undefined) {
			headers.set(key, String(value));
		}
	}

	const url = new URL(request.url || "/", nodeRequestOrigin(request));
	const method = request.method || "GET";
	const init = { method, headers };
	if (method !== "GET" && method !== "HEAD") {
		init.body = await readNodeRequestBody(request);
		init.duplex = "half";
	}

	return new Request(url, init);
}

function nodeRequestOrigin(request) {
	const forwardedProto = request.headers?.["x-forwarded-proto"];
	const proto = Array.isArray(forwardedProto)
		? forwardedProto[0]
		: String(forwardedProto || "https").split(",")[0].trim();
	const host = request.headers?.host || "localhost";
	return `${proto || "https"}://${host}`;
}

async function readNodeRequestBody(request) {
	if (request.body !== undefined && request.body !== null) {
		if (typeof request.body === "string" || Buffer.isBuffer(request.body) || request.body instanceof Uint8Array) {
			return request.body;
		}
		return JSON.stringify(request.body);
	}

	const chunks = [];
	for await (const chunk of request) {
		chunks.push(typeof chunk === "string" ? Buffer.from(chunk) : chunk);
	}
	return Buffer.concat(chunks);
}

async function sendNodeResponse(response, fetchResponse) {
	response.statusCode = fetchResponse.status;
	response.statusMessage = fetchResponse.statusText;
	fetchResponse.headers.forEach((value, key) => {
		response.setHeader(key, value);
	});

	if (!fetchResponse.body) {
		response.end();
		return;
	}

	const reader = fetchResponse.body.getReader();
	try {
		while (true) {
			const { done, value } = await reader.read();
			if (done) break;
			if (!response.write(Buffer.from(value))) {
				await new Promise((resolve) => response.once("drain", resolve));
			}
		}
	} finally {
		response.end();
		reader.releaseLock();
	}
}

async function handleOpenAI(request) {
	const requestId = ocId("req");
	const auth = authenticate(request);
	if (auth.error) return auth.error;

	const input = await readJson(request);
	if (input.error) return input.error;

	const { model, messages, stream, tools, tool_choice } = input.body;

	const sessionId = getSession(auth.user);
	const msgSummary = (messages || []).map((msg) => ({
		role: msg.role,
		len: typeof msg.content === "string" ? msg.content.length : JSON.stringify(msg.content || "").length,
	}));
	console.log("[OAI]", new Date().toISOString(), auth.user, model, stream ? "stream" : "sync", "msgs:", JSON.stringify(msgSummary));

	const upstreamModel = deepSeekNeedsImageFallback(model, messages) ? IMAGE_FALLBACK_MODEL : model;

	// 纯文字请求走 DeepSeek 时,把历史里的图片剥掉(DeepSeek 无法解析 image_url)。
	const outgoingMessages = upstreamModel === model ? stripImagesForDeepSeek(messages) : messages;

	const transformedMessages = injectReasoningContent(upstreamModel, outgoingMessages);
	const zenReq = buildZenRequest(upstreamModel, transformedMessages, stream, tools, tool_choice, sessionId);
	logZenRequest(requestId, "openai", model, stream, auth.user, zenReq, messages?.length || 0);

	let upstream;
	try {
		upstream = await fetchZen(zenReq, requestId, upstreamModel, stream);
	} catch (error) {
		debugLog("[ZEN FETCH ERROR]", { requestId, model: upstreamModel, stream: !!stream, message: error?.message || String(error) });
		return upstreamErrorResponse(error);
	}

	if (stream) return openAIStreamResponse(upstream, requestId, model);
	return openAIFullResponse(upstream, requestId, model);
}

const IP_PROVIDERS = [
	"https://api.ipquery.io",
	"http://ip-api.com/json",
];

const IPV4_REGEX = /\b\d{1,3}(?:\.\d{1,3}){3}\b/;

async function ipResponse() {
	const results = await Promise.allSettled(
		IP_PROVIDERS.map((url) => fetchIPFrom(url))
	);

	for (const result of results) {
		if (result.status === "fulfilled" && result.value) {
			return jsonResponse({ ip: result.value.ip, source: result.value.source });
		}
	}

	return upstreamErrorResponse(new Error("all IP providers failed"));
}

async function fetchIPFrom(url) {
	const controller = new AbortController();
	const timeout = setTimeout(() => controller.abort("timeout"), 10 * 1000);

	try {
		const response = await fetch(url, { signal: controller.signal });
		if (!response.ok) return null;

		const body = await response.text();
		const match = IPV4_REGEX.exec(body);
		const ip = match ? match[0] : "";
		if (!isValidIPv4(ip)) return null;

		return { ip, source: url };
	} catch {
		return null;
	} finally {
		clearTimeout(timeout);
	}
}

function isValidIPv4(ip) {
	if (typeof ip !== "string" || !ip) return false;
	const parts = ip.split(".");
	if (parts.length !== 4) return false;
	return parts.every((part) => /^\d{1,3}$/.test(part) && Number(part) >= 0 && Number(part) <= 255);
}

function healthResponse() {
	return jsonResponse({
		status: "ok",
		version: PROXY_VERSION,
		endpoints: ["/v1/chat/completions", "/chat/completions", "/v1/models", "/models", "/health", "/ip"],
	});
}

async function modelsResponse() {
	try {
		return jsonResponse({
			object: "list",
			data: await getAvailableModels(),
		});
	} catch (error) {
		debugLog("[MODEL LIST ERROR]", { message: error?.message || String(error) });
		return upstreamErrorResponse(error);
	}
}

async function getAvailableModels() {
	if (cachedModels) return cachedModels;
	cachedModels = await fetchZenModels();
	return cachedModels;
}

async function fetchZenModels() {
	const controller = new AbortController();
	const timeout = setTimeout(() => controller.abort("timeout"), FETCH_TIMEOUT_MS);

	try {
		const started = Date.now();
		const response = await fetch(ZEN_MODELS_URL, {
			method: "GET",
			headers: {
				"Accept": "application/json",
				"Authorization": "Bearer public",
				"User-Agent": `opencode/${OC_VERSION} ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13`,
			},
			signal: controller.signal,
		});
		const raw = await response.text();
		const parsed = safeJsonParse(raw);

		if (!response.ok) throw new Error(`Model list returned HTTP ${response.status}`);
		if (!Array.isArray(parsed?.data)) throw new Error("Invalid model list response");

		const models = parsed.data.filter((item) => isAllowedModelId(item?.id));

		if (!models.length) throw new Error("No allowed models returned from upstream");

		debugLog("[MODEL LIST]", { status: response.status, ms: Date.now() - started, total: parsed.data.length, allowed: models.length });
		return models;
	} catch (error) {
		if (error?.name === "AbortError" || error === "timeout") throw new Error("timeout");
		throw error;
	} finally {
		clearTimeout(timeout);
	}
}

function isAllowedModelId(id) {
	return typeof id === "string" && (id === "big-pickle" || id.endsWith("-free"));
}

function cachedModelCount() {
	return Array.isArray(cachedModels) ? cachedModels.length : 0;
}

const reasoningPlaceholder = " ";
const deepSeekRegex = /deepseek/i;

// DeepSeek 是否需要图片回退:仅当最近一条 user 消息带图时路由到 mimo(识别走 mimo)。
// 历史残留的图片不影响路由:纯文字追问仍走 DeepSeek(思考走 deepseek)。
function lastUserMessageHasImage(messages) {
	if (!Array.isArray(messages)) return false;
	for (let i = messages.length - 1; i >= 0; i--) {
		const msg = messages[i];
		if (msg?.role !== "user") continue;
		if (!Array.isArray(msg.content)) return false;
		return msg.content.some((part) => part?.type === "image_url" || part?.type === "image");
	}
	return false;
}

function deepSeekNeedsImageFallback(model, messages) {
	return deepSeekRegex.test(model) && lastUserMessageHasImage(messages);
}

// DeepSeek 上游对整段 messages 里任何一处的 image_url 都会反序列化报错。
// 纯文字追问会带上前一轮的图片消息,发给 DeepSeek 前把历史里的图片剥离掉。
function stripImagesForDeepSeek(messages) {
	if (!Array.isArray(messages)) return messages;
	const hasImage = messages.some(
		(msg) =>
			Array.isArray(msg?.content) &&
			msg.content.some((part) => part?.type === "image_url" || part?.type === "image")
	);
	if (!hasImage) return messages;

	return messages.map((msg) => {
		if (!Array.isArray(msg.content)) return msg;
		const parts = msg.content.filter((part) => !(part?.type === "image_url" || part?.type === "image"));
		if (parts.length === msg.content.length) return msg;

		let content;
		if (parts.length === 0) content = "[图片]";
		else if (parts.every((p) => p?.type === "text")) content = parts.map((p) => p.text).join("");
		else content = parts;
		return { ...msg, content };
	});
}

function injectReasoningContent(model, messages) {
	if (!messages) return messages;

	const changed = messages.some((msg) =>
		msg?.role === "assistant"
		&& !msg?.reasoning_content
		&& deepSeekRegex.test(model)
	);

	if (!changed) return messages;

	const next = JSON.parse(JSON.stringify(messages));
	for (const msg of next) {
		if (msg.role === "assistant" && !msg.reasoning_content && deepSeekRegex.test(model)) {
			msg.reasoning_content = reasoningPlaceholder;
		}
	}
	return next;
}

function buildZenRequest(model, messages, stream, tools, toolChoice, sessionId) {
	const reqBody = { model, messages, stream: !!stream };
	// 按实际发送的模型判断:路由到图片模型时跳过 DS 专属的 reasoning_effort
	const effort = ["low", "medium", "high", "max"].includes(REASONING_EFFORT) ? REASONING_EFFORT : "high";
	if (deepSeekRegex.test(model)) {
		reqBody.reasoning_effort = effort;
	}
	if (tools?.length) reqBody.tools = tools;
	if (toolChoice) reqBody.tool_choice = toolChoice;

	return {
		body: JSON.stringify(reqBody),
		headers: {
			"Content-Type": "application/json",
			"Authorization": "Bearer public",
			"User-Agent": `opencode/${OC_VERSION} ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13`,
			"x-opencode-client": "desktop",
			"x-opencode-project": "global",
			"x-opencode-request": ocId("msg"),
			"x-opencode-session": sessionId,
		},
		messageCount: messages?.length || 0,
	};
}

async function fetchZen(zenReq, requestId, model, stream) {
	const controller = new AbortController();
	const timeout = setTimeout(() => controller.abort("timeout"), FETCH_TIMEOUT_MS);

	try {
		const started = Date.now();
		const response = await fetch(ZEN_URL, {
			method: "POST",
			headers: zenReq.headers,
			body: zenReq.body,
			signal: controller.signal,
		});
		logZenResponse({ requestId, model, stream: !!stream, status: response.status, ok: response.ok, ms: Date.now() - started });
		return response;
	} catch (error) {
		if (error?.name === "AbortError" || error === "timeout") throw new Error("timeout");
		throw error;
	} finally {
		clearTimeout(timeout);
	}
}

async function openAIFullResponse(upstream, requestId, model) {
	const raw = await upstream.text();
	const data = safeJsonParse(raw);
	const zenError = parseZenError(raw);
	logUpstreamBody(requestId, model, upstream.status, raw, zenError);

	if (upstream.status === 429 || zenError) {
		return openAIErrorResponse(`${zenError?.message || "Rate limit exceeded"} (free model rate limit)`, "rate_limit_error", 429, "rate_limit_exceeded");
	}

	if (data?.choices) {
		return jsonResponse(normalizeOpenAIFullData(data, model), upstream.status);
	}

	return new Response(raw, {
		status: upstream.status,
		headers: mergeHeaders({ "Content-Type": upstream.headers.get("Content-Type") || "application/json; charset=utf-8" }),
	});
}

async function openAIStreamResponse(upstream, requestId, model) {
	if (!upstream.body) {
		return openAIErrorResponse("Empty response from upstream", "upstream_error", 502);
	}

	const reader = upstream.body.getReader();
	const first = await reader.read();
	if (first.done) {
		return openAIErrorResponse("Empty response from upstream", "upstream_error", 502);
	}

	const firstText = new TextDecoder().decode(first.value);
	const zenError = parseZenError(firstText);
	logUpstreamBody(requestId, model, upstream.status, firstText, zenError, true);
	if (upstream.status === 429 || zenError) {
		await reader.cancel().catch(() => { });
		return openAIErrorResponse(`${zenError?.message || "Rate limit exceeded"} (free model rate limit)`, "rate_limit_error", 429, "rate_limit_exceeded");
	}

	const encoder = new TextEncoder();
	const decoder = new TextDecoder();
	const normalizer = createOpenAIStreamNormalizer(model);

	const stream = new ReadableStream({
		async start(controller) {
			let buffer = "";
			let doneSent = false;

			const enqueue = (text) => controller.enqueue(encoder.encode(text));
			const sendData = (payload) => enqueue(`data: ${typeof payload === "string" ? payload : JSON.stringify(payload)}\n\n`);
			const sendDone = () => {
				if (doneSent) return;
				doneSent = true;
				sendData("[DONE]");
			};
			const processLine = (rawLine) => {
				const line = rawLine.trimEnd();
				if (!line.startsWith("data:")) return;

				const payload = line.slice(5).trim();
				if (!payload) return;
				if (payload === "[DONE]") { sendDone(); return; }
				if (doneSent) return;

				const parsed = safeJsonParse(payload);
				if (!parsed) return;

				const normalized = normalizer.normalize(parsed);
				if (normalized) sendData(normalized);
			};
			const processChunk = (chunk) => {
				buffer += decoder.decode(chunk, { stream: true });
				const lines = buffer.split("\n");
				buffer = lines.pop() || "";
				for (const line of lines) processLine(line);
			};

			try {
				processChunk(first.value);
				while (!doneSent) {
					const { done, value } = await reader.read();
					if (done) break;
					processChunk(value);
				}

				const tail = decoder.decode();
				if (tail) buffer += tail;
				if (buffer) processLine(buffer);
				if (doneSent) await reader.cancel().catch(() => { });
				sendDone();
				controller.close();
			} catch (error) {
				controller.error(error);
			}
		},
		cancel(reason) {
			return reader.cancel(reason);
		},
	});

	return new Response(stream, {
		status: upstream.status,
		headers: mergeHeaders(SSE_HEADERS),
	});
}

function normalizeOpenAIFullData(data, model) {
	const next = { ...data };
	if (model) next.model = model;
	if (!Array.isArray(next.choices)) return next;

	next.choices = next.choices.map((choice) => {
		if (!choice?.message) return choice;

		const message = { ...choice.message };
		normalizeReasoningField(message);
		if (!SHOW_REASONING) delete message.reasoning_content;

		if (typeof message.content === "string") {
			if (SHOW_REASONING && message.reasoning_content == null) {
				const reasoning = extractThinkBlocks(message.content);
				if (reasoning) message.reasoning_content = reasoning;
			}
			const visibleContent = stripThinkBlocks(message.content);
			if (visibleContent !== message.content) message.content = visibleContent;
		}

		return { ...choice, message };
	});

	return next;
}

function createOpenAIStreamNormalizer(model) {
	const contentStates = new Map();

	return {
		normalize(chunk) {
			if (!chunk || !Array.isArray(chunk.choices)) return null;
			if (!chunk.choices.length && chunk.cost != null) return null;

			const next = { ...chunk };
			delete next.cost;
			if (model) next.model = model;
			next.choices = chunk.choices
				.map((choice) => normalizeOpenAIStreamChoice(choice, contentStates))
				.filter(Boolean);

			if (!next.choices.length && !next.usage) return null;
			return next;
		},
	};
}

function normalizeOpenAIStreamChoice(choice, contentStates) {
	if (!choice?.delta) return choice;

	const delta = { ...choice.delta };
	normalizeReasoningField(delta);
	if (!SHOW_REASONING) delete delta.reasoning_content;

	if (typeof delta.content === "string") {
		const state = getThinkState(contentStates, choice.index ?? 0);
		const visibleContent = stripThinkStreamText(delta.content, state);
		if (visibleContent) delta.content = visibleContent;
		else delete delta.content;
	}

	if (!Object.keys(delta).length && !choice.finish_reason) return null;
	return { ...choice, delta };
}

function normalizeReasoningField(target) {
	if (!target || typeof target !== "object") return;
	if (typeof target.reasoning === "string" && target.reasoning && target.reasoning_content == null) {
		target.reasoning_content = target.reasoning;
	}
	delete target.reasoning;
}

function extractThinkBlocks(text) {
	const matches = [];
	const pattern = / thinking([\s\S]*?)<\/think>/gi;
	let match;
	while ((match = pattern.exec(text)) !== null) {
		const content = match[1].trim();
		if (content) matches.push(content);
	}
	return matches.join("\n");
}

function stripThinkBlocks(text) {
	if (!/<\/?think>/i.test(text)) return text;
	const state = createThinkState();
	return stripThinkStreamText(text, state);
}

function getThinkState(states, key) {
	const stateKey = String(key);
	if (!states.has(stateKey)) states.set(stateKey, createThinkState());
	return states.get(stateKey);
}

function createThinkState() {
	return { inThink: false, emittedContent: false, removedThink: false };
}

function stripThinkStreamText(text, state) {
	let output = "";
	let cursor = 0;
	const lower = text.toLowerCase();

	while (cursor < text.length) {
		if (state.inThink) {
			const end = lower.indexOf("</think>", cursor);
			if (end === -1) break;
			cursor = end + "</think>".length;
			state.inThink = false;
			state.removedThink = true;
			continue;
		}

		const start = lower.indexOf("<think>", cursor);
		if (start === -1) {
			output += text.slice(cursor);
			break;
		}

		output += text.slice(cursor, start);
		cursor = start + "<think>".length;
		state.inThink = true;
		state.removedThink = true;
	}

	if (state.removedThink && !state.emittedContent && output) output = output.replace(/^\s+/, "");
	if (output) state.emittedContent = true;
	return output;
}

function debugLog(label, payload) {
	if (getEnv("DEBUG") !== "true") return;
	console.log(label, JSON.stringify(payload));
}

function logZenRequest(requestId, format, model, stream, user, zenReq, messageCount) {
	debugLog("[ZEN REQ]", { requestId, format, user, model, stream: !!stream, messageCount, bodyBytes: byteLength(zenReq.body), ocRequest: shortId(zenReq.headers["x-opencode-request"]), ocSession: shortId(zenReq.headers["x-opencode-session"]) });
}

function logZenResponse(payload) {
	const { status } = payload;
	if (!getEnv("DEBUG") && status < 400) return;
	console.log("[ZEN RES]", JSON.stringify(payload));
}

function logUpstreamBody(requestId, model, status, raw, zenError, firstChunk = false) {
	const body = String(raw || "");
	const shouldLog = getEnv("DEBUG") || Boolean(zenError) || status >= 400;
	if (!shouldLog) return;

	const payload = { requestId, model, status, firstChunk, chars: body.length };
	if (zenError) payload.zenError = { message: zenError.message, type: zenError.type };
	if (shouldLog) payload.preview = previewText(body);

	console.log("[ZEN BODY]", JSON.stringify(payload));
}

function byteLength(text) {
	return new TextEncoder().encode(String(text || "")).length;
}

function shortId(id) {
	const text = String(id || "");
	if (text.length <= 16) return text;
	return `${text.slice(0, 8)}...${text.slice(-6)}`;
}

function previewText(text, max = 800) {
	return String(text || "").replace(/\s+/g, " ").slice(0, max);
}

// ---- HTTP 代理接口 ----
// 用法：
//   1. /proxy?url=<encoded-url>（任意方法，URL 在 query 参数）
//   2. /proxy/https://target/...（URL 直接跟在路径后）
//   3. 标准 HTTP 代理 absolute-form（curl -x https://<host> 直达，目标 host 非本域名时自动识别）
// 认证与 AI 接口一致：Authorization: Bearer <API_KEY> / X-API-Key / Proxy-Authorization

const PROXY_BLOCKED_HOSTS = /^(localhost|0\.0\.0\.0|::1|127\.|10\.|192\.168\.|169\.254\.|172\.(1[6-9]|2\d|3[01])\.)/i;

function requestHost(request) {
	return (request.headers.get("x-forwarded-host") || request.headers.get("host") || "")
		.split(":")[0]
		.toLowerCase();
}

function isProxyTargetAllowed(targetUrl) {
	const url = new URL(targetUrl);
	if (url.protocol !== "http:" && url.protocol !== "https:") return false;
	const host = url.hostname.toLowerCase();
	if (PROXY_BLOCKED_HOSTS.test(host)) return false;
	if (host.includes(":") && !host.startsWith("[")) return false; // 原始 IPv6 一律拒绝
	return true;
}

async function handleProxy(request, target) {
	let url;
	try {
		url = new URL(target);
	} catch {
		return jsonResponse({ error: { message: "Invalid target URL" } }, 400);
	}

	if (!isProxyTargetAllowed(url.toString())) {
		return jsonResponse({ error: { message: "Target not allowed" } }, 403);
	}

	const headers = new Headers();
	for (const [key, value] of request.headers) {
		const lower = key.toLowerCase();
		if (
			lower === "host" ||
			lower === "content-length" ||
			lower === "connection" ||
			lower === "keep-alive" ||
			lower === "te" ||
			lower === "trailer" ||
			lower === "transfer-encoding" ||
			lower === "upgrade" ||
			lower === "proxy-authorization" ||
			lower === "proxy-connection" ||
			lower === "accept-encoding" ||
			lower.startsWith("cf-") ||
			lower.startsWith("x-vercel-") ||
			lower.startsWith("x-forwarded-") ||
			lower.startsWith("x-real-")
		) {
			continue;
		}
		headers.set(key, value);
	}

	const init = { method: request.method, headers, redirect: "follow" };
	if (request.method !== "GET" && request.method !== "HEAD") {
		init.body = request.body;
		init.duplex = "half";
	}

	let upstream;
	try {
		upstream = await fetch(url.toString(), init);
	} catch (error) {
		console.log("[PROXY ERROR]", error?.message || error);
		return jsonResponse({ error: { message: `Proxy upstream error: ${error?.message || "fetch failed"}` } }, 502);
	}

	const resHeaders = new Headers();
	for (const [key, value] of upstream.headers) {
		const lower = key.toLowerCase();
		if (
			lower === "content-encoding" ||
			lower === "content-length" ||
			lower === "transfer-encoding" ||
			lower === "connection" ||
			lower === "keep-alive"
		) {
			continue;
		}
		resHeaders.set(key, value);
	}

	return new Response(upstream.body, { status: upstream.status, headers: resHeaders });
}

function authenticate(request) {
	const apiKey = getEnv("API_KEY");
	if (!apiKey) return { user: "anonymous" };

	let header =
		request.headers.get("authorization") ||
		request.headers.get("x-api-key") ||
		request.headers.get("proxy-authorization") ||
		"";

	// 兼容代理客户端 Basic 认证（curl -x -U user:pass），密码等于 API_KEY 即通过
	if (header.toLowerCase().startsWith("basic ")) {
		try {
			const decoded = atob(header.slice(6).trim());
			const pass = decoded.includes(":") ? decoded.slice(decoded.indexOf(":") + 1) : decoded;
			if (pass === apiKey) return { user: "user-proxy" };
		} catch {
			/* fallthrough */
		}
		return { error: openAIErrorResponse("Invalid API key", "authentication_error", 401) };
	}

	const token = header.toLowerCase().startsWith("bearer ") ? header.slice(7).trim() : header.trim();

	if (token === apiKey) return { user: "user-default" };

	return { error: openAIErrorResponse("Invalid API key", "authentication_error", 401) };
}

// Zen 免费层要求 session ID 为 ses_ + 26 位小写十六进制
function zenSessionID() {
	const bytes = new Uint8Array(13);
	globalThis.crypto.getRandomValues(bytes);
	return 'ses_' + [...bytes].map(b => b.toString(16).padStart(2, '0')).join('');
}

function getSession(user) {
	const now = Date.now();
	const existing = userSessions.get(user);
	if (!existing || now - existing.ts > 30 * 60 * 1000) {
		const next = { id: zenSessionID(), ts: now };
		userSessions.set(user, next);
		return next.id;
	}
	existing.ts = now;
	return existing.id;
}

async function readJson(request) {
	try {
		return { body: await request.json() };
	} catch {
		return { error: openAIErrorResponse("Invalid JSON body", "invalid_request_error", 400) };
	}
}

function parseZenError(raw) {
	const text = String(raw || "").trim();
	if (!text.startsWith("{")) return null;
	if (!text.includes("FreeUsageLimitError") && !text.includes('"error"') && !text.includes('"type"')) return null;

	const parsed = safeJsonParse(text);
	if (!parsed || (!parsed.error && parsed.type !== "error")) return null;

	return { message: parsed.error?.message || parsed.message || "Rate limit exceeded", type: parsed.error?.type || parsed.type || "upstream_error" };
}

function upstreamErrorResponse(error) {
	const timeout = error?.message === "timeout";
	const message = timeout ? "Upstream timeout" : `Upstream error: ${error?.message || error}`;
	const type = timeout ? "timeout_error" : "upstream_error";
	const status = timeout ? 504 : 502;
	return openAIErrorResponse(message, type, status);
}

function openAIErrorResponse(message, type, status, code) {
	return jsonResponse({ error: { message, type, ...(code ? { code } : {}) } }, status);
}

function jsonResponse(data, status = 200, headers = {}) {
	return new Response(JSON.stringify(data), { status, headers: mergeHeaders(JSON_HEADERS, headers) });
}

function mergeHeaders(...sets) {
	const headers = new Headers(CORS_HEADERS);
	for (const set of sets) {
		for (const [key, value] of Object.entries(set || {})) {
			headers.set(key, value);
		}
	}
	return headers;
}

function safeJsonParse(text) {
	try {
		return JSON.parse(text);
	} catch {
		return null;
	}
}

function ocId(prefix) {
	const bytes = new Uint8Array(12);
	crypto.getRandomValues(bytes);
	let binary = "";
	for (const byte of bytes) binary += String.fromCharCode(byte);
	const rnd = btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "").slice(0, 16);
	return `${prefix}_${Date.now().toString(16)}${rnd}`;
}

// 供 Cloudflare Worker 等 Web 运行时直接复用（worker/index.js 入口）
export { handleRequest };
