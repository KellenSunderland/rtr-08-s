export interface Config {
  baseUrl: string;
  token: string;
  scenario: string;
}
const KEY = "argus.console.config";
/** Used when no token is saved or injected via VITE_GATEWAY_TOKEN, so `pnpm dev` works out of the box. */
const DEV_DEFAULT_TOKEN = "argus-dev-2f9b7c41d8e65a03b1c7";
export const DEFAULT_CONFIG: Config = {
  baseUrl: (import.meta.env.VITE_GATEWAY_URL as string | undefined) ?? "http://localhost:8080",
  token: (import.meta.env.VITE_GATEWAY_TOKEN as string | undefined) ?? DEV_DEFAULT_TOKEN,
  scenario: "",
};

export function loadConfig(storage: Pick<Storage, "getItem"> = localStorage): Config {
  try {
    const raw = storage.getItem(KEY);
    if (!raw) return DEFAULT_CONFIG;
    const p = JSON.parse(raw) as Partial<Config>;
    return {
      baseUrl: typeof p.baseUrl === "string" ? p.baseUrl : DEFAULT_CONFIG.baseUrl,
      token: typeof p.token === "string" ? p.token : DEFAULT_CONFIG.token,
      scenario: typeof p.scenario === "string" ? p.scenario : "",
    };
  } catch {
    return DEFAULT_CONFIG;
  }
}
export function saveConfig(c: Config, storage: Pick<Storage, "setItem"> = localStorage): void {
  storage.setItem(KEY, JSON.stringify(c));
}
