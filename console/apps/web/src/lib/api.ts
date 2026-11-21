import { z } from "zod";
import {
  CreateSessionRequest,
  CreateStreamRequest,
  HistoryResult,
  ScenarioInfo,
  SessionInfo,
  StreamInfo,
} from "@argus/protocol";
import { intHeader } from "./overlay";

export interface ApiConfig {
  baseUrl: string;
  token: string;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message);
  }
}

export class ApiClient {
  private baseUrl: string;
  constructor(
    cfg: ApiConfig,
    private fetchImpl: typeof fetch = (...a) => fetch(...a),
  ) {
    this.baseUrl = cfg.baseUrl.replace(/\/+$/, "");
    this.token = cfg.token;
  }
  private token: string;

  private headers(extra?: Record<string, string>): Record<string, string> {
    return { ...(this.token ? { Authorization: `Bearer ${this.token}` } : {}), ...extra };
  }

  private async request(path: string, init: RequestInit = {}): Promise<Response> {
    const res = await this.fetchImpl(this.baseUrl + path, {
      ...init,
      headers: this.headers(init.headers as Record<string, string> | undefined),
    });
    if (!res.ok) {
      let code = "internal";
      let message = res.statusText;
      try {
        const body = (await res.json()) as { code?: string; message?: string };
        code = body.code ?? code;
        message = body.message ?? message;
      } catch {
        /* non-JSON error body */
      }
      throw new ApiError(res.status, code, message || `HTTP ${res.status}`);
    }
    return res;
  }

  private async json<T>(path: string, schema: z.ZodType<T>, init?: RequestInit): Promise<T> {
    const res = await this.request(path, init);
    return schema.parse(await res.json());
  }

  listStreams = () => this.json("/v1/streams", z.array(StreamInfo));
  getStream = (id: string) => this.json(`/v1/streams/${encodeURIComponent(id)}`, StreamInfo);
  createStream(req: z.input<typeof CreateStreamRequest>) {
    const body = CreateStreamRequest.parse(req);
    return this.json("/v1/streams", StreamInfo, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  }
  async deleteStream(id: string): Promise<void> {
    await this.request(`/v1/streams/${encodeURIComponent(id)}`, { method: "DELETE" });
  }
  listScenarios = () => this.json("/v1/scenarios", z.array(ScenarioInfo));
  createSession(req: z.input<typeof CreateSessionRequest>) {
    const body = CreateSessionRequest.parse(req);
    return this.json("/v1/sessions", SessionInfo, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  }
  async deleteSession(id: string): Promise<void> {
    await this.request(`/v1/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
  }
  history(p: { since_ms: number; until_ms: number; stream_id?: string; q?: string; limit?: number }) {
    const qs = new URLSearchParams({ since_ms: String(p.since_ms), until_ms: String(p.until_ms) });
    if (p.stream_id) qs.set("stream_id", p.stream_id);
    if (p.q) qs.set("q", p.q);
    if (p.limit) qs.set("limit", String(p.limit));
    return this.json(
      `/v1/history?${qs}`,
      HistoryResult.pick({ events: true, summaries: true, truncated: true }),
    );
  }

  /** Relative path (no host) of a frame; tiles fetch it with auth and show a blob URL. */
  static framePath(id: string, opts: { ts_ms?: number; max_w?: number } = {}): string {
    const qs = new URLSearchParams();
    if (opts.max_w) qs.set("max_w", String(opts.max_w));
    if (opts.ts_ms !== undefined) qs.set("ts_ms", String(Math.max(0, Math.round(opts.ts_ms))));
    const s = qs.toString();
    return `/v1/streams/${encodeURIComponent(id)}/frame${s ? `?${s}` : ""}`;
  }
  async fetchFrame(
    id: string,
    opts: { ts_ms?: number; max_w?: number } = {},
    signal?: AbortSignal,
  ): Promise<Blob> {
    const res = await this.request(ApiClient.framePath(id, opts), { signal });
    return res.blob();
  }

  /** Like fetchFrame, plus the timestamp of the frame actually returned (nearest to `ts_ms`). */
  async fetchFrameWithTs(
    id: string,
    opts: { ts_ms?: number; max_w?: number } = {},
    signal?: AbortSignal,
  ): Promise<{ blob: Blob; tsMs: number | null }> {
    const res = await this.request(ApiClient.framePath(id, opts), { signal });
    return { blob: await res.blob(), tsMs: intHeader(res.headers, "X-Frame-Ts-Ms") };
  }

  /**
   * Browsers cannot set Authorization on WebSocket handshakes, so the token is passed as
   * `?token=`. (Protocol gap: openapi.yaml does not document this; see report.)
   */
  wsUrl(sessionId: string): string {
    const u = new URL(this.baseUrl + `/v1/sessions/${encodeURIComponent(sessionId)}/ws`);
    u.protocol = u.protocol === "https:" ? "wss:" : "ws:";
    if (this.token) u.searchParams.set("token", this.token);
    return u.toString();
  }
}
