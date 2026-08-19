// Cloudflare Worker 入口：复用 api/index.js 的完整处理逻辑（Web Request API）
// 环境变量通过 Worker 的 env 参数注入，由 api/index.js 的 getEnv() 读取
import { handleRequest } from "../api/index.js";

export default {
	async fetch(request, env) {
		globalThis.__OC_ENV__ = env;
		return handleRequest(request);
	},
};