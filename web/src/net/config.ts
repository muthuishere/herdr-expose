/**
 * `GET /v1/config` — the ONLY way the browser can learn why a WebSocket failed.
 *
 * The server rejects an unauthenticated upgrade with HTTP 401 BEFORE upgrading.
 * A browser's WebSocket API cannot read the status code of a failed handshake:
 * no close code is delivered, only a generic error/close. So "treat 1008/4401/
 * 4403 as an auth failure" never fires over the tunnel, and the client cannot
 * tell "needs pairing" from "network is down" — it retries forever.
 *
 * `/v1/config` is a plain HTTP GET, so it CAN report status. It carries:
 *   auth_required — false only for a genuine loopback listener+peer with a
 *                   pinned Host (SPEC AMENDMENTS 7 / F1).
 *   authenticated — whether the token we presented (if any) is valid right now.
 *
 * We probe it before the first connect and again after every socket failure.
 * That turns an unreadable handshake into a decidable fact.
 */

import { getToken } from './auth'

export interface ServerConfig {
  api?: string
  mode?: string
  stream?: string
  version?: string
  auth_required: boolean
  authenticated: boolean
  exposure?: { healthy?: boolean; url?: string }
  limits?: { min_cols?: number; min_rows?: number }
  ui?: { theme?: string; default_view?: string }
}

/** The probe's verdict. `unreachable` is a NETWORK fact, not an auth fact. */
export type ConfigProbe =
  | { ok: true; config: ServerConfig; needsPairing: boolean }
  | { ok: false; unreachable: true }

const TIMEOUT_MS = 6000

/**
 * Never throws. A failure to fetch is reported as `unreachable`, which callers
 * MUST treat as "the network is down", never as "you are not paired" — guessing
 * auth from a network error is how you log someone out of a working session.
 */
export async function probeConfig(): Promise<ConfigProbe> {
  const ctl = typeof AbortController !== 'undefined' ? new AbortController() : null
  const timer = ctl ? setTimeout(() => ctl.abort(), TIMEOUT_MS) : undefined
  try {
    const token = getToken()
    const res = await fetch('/v1/config', {
      method: 'GET',
      headers: token ? { Authorization: `Bearer ${token}` } : undefined,
      credentials: 'omit',
      cache: 'no-store',
      signal: ctl?.signal,
    })
    // A 401 here still ANSWERS the question: the server is up and refusing us.
    if (res.status === 401 || res.status === 403) {
      return {
        ok: true,
        config: { auth_required: true, authenticated: false },
        needsPairing: true,
      }
    }
    if (!res.ok) return { ok: false, unreachable: true }
    const cfg = (await res.json()) as ServerConfig
    if (!cfg || typeof cfg !== 'object') return { ok: false, unreachable: true }
    const authRequired = cfg.auth_required !== false
    const authenticated = cfg.authenticated === true
    return { ok: true, config: cfg, needsPairing: authRequired && !authenticated }
  } catch {
    return { ok: false, unreachable: true }
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}
