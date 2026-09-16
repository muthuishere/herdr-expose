/**
 * Pairing — SPEC §4 `POST /v1/pair`, B5 (6-char code, single use, TTL 10 min).
 *
 * The QR is shown ONLY on the local machine and encodes
 * `https://<host>/?pair=<code>`, so the phone lands on the PWA itself and
 * exchanges the code for a long-lived device token. That is the whole flow:
 * the phone never sees the server token, only a device token it can be
 * individually revoked from.
 *
 * As with `?token=`, the code is stripped from the address bar immediately —
 * a pairing URL in a screenshot or in history is a credential.
 */

import { setToken } from './auth'

export interface PairResult {
  ok: boolean
  error?: string
}

let consumed = false
let memo: string | null = null

/**
 * Read and CONSUME `?pair=` from the URL.
 *
 * IDEMPOTENT ON PURPOSE. Stripping the query string is a side effect, and this
 * is read from a `useState` initializer — which React StrictMode invokes twice.
 * A naive implementation consumes the code on the first call and hands the
 * second call `null`, which is the value React keeps, so the deep link
 * silently does nothing in dev. Memoise instead.
 */
export function takePairCodeFromUrl(): string | null {
  if (consumed) return memo
  consumed = true
  try {
    const url = new URL(window.location.href)
    const code = url.searchParams.get('pair')
    if (!code) {
      memo = null
      return null
    }
    url.searchParams.delete('pair')
    window.history.replaceState({}, '', url.pathname + url.search + url.hash)
    memo = normalizeCode(code)
  } catch {
    memo = null
  }
  return memo
}

/** Forget the consumed code once it has been redeemed (or abandoned). */
export function clearPairCode(): void {
  memo = null
}

/** Codes are 6 chars, case-insensitive, entered on a phone keyboard. */
export function normalizeCode(raw: string): string {
  return raw.trim().toUpperCase().replace(/[^A-Z0-9]/g, '').slice(0, 6)
}

export async function redeemPairCode(code: string): Promise<PairResult> {
  const c = normalizeCode(code)
  if (c.length !== 6) return { ok: false, error: 'A pairing code is 6 characters.' }
  try {
    const res = await fetch('/v1/pair', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code: c }),
      credentials: 'omit',
    })
    if (!res.ok) {
      // The server must not distinguish "wrong" from "expired" in a way that
      // helps a guesser; we just relay whatever it chose to say.
      const text = await res.text().catch(() => '')
      return {
        ok: false,
        error:
          res.status === 429
            ? 'Too many attempts. Wait a moment and try again.'
            : safeMessage(text) || 'That code was not accepted. Ask for a fresh one.',
      }
    }
    const body = (await res.json()) as { token?: string }
    if (!body?.token) return { ok: false, error: 'The server did not return a device token.' }
    setToken(body.token)
    return { ok: true }
  } catch {
    return { ok: false, error: 'Could not reach the server. Is herdr-expose running?' }
  }
}

function safeMessage(text: string): string {
  try {
    const j = JSON.parse(text) as { error?: string }
    return typeof j.error === 'string' ? j.error.slice(0, 160) : ''
  } catch {
    return ''
  }
}
