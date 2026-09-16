/**
 * Auth (SPEC §8 / §4). The token arrives as `?token=` on first load (from the
 * QR that `herdr-expose pair` prints), is persisted to localStorage, and is
 * then sent in the WS URL.
 *
 * We strip the token from the address bar immediately so it does not end up in
 * a screenshot, a shared link, or the browser history.
 */

const KEY = 'herdr-expose.token'

export function takeTokenFromUrl(): void {
  const url = new URL(window.location.href)
  const t = url.searchParams.get('token')
  if (!t) return
  try {
    localStorage.setItem(KEY, t)
  } catch {
    /* private mode: fall through, the in-memory copy below still works */
  }
  memo = t
  url.searchParams.delete('token')
  window.history.replaceState({}, '', url.pathname + url.search + url.hash)
}

let memo: string | null = null

export function getToken(): string | null {
  if (memo) return memo
  try {
    memo = localStorage.getItem(KEY)
  } catch {
    memo = null
  }
  return memo
}

export function setToken(t: string): void {
  memo = t
  try {
    localStorage.setItem(KEY, t)
  } catch {
    /* ignore */
  }
}

export function clearToken(): void {
  memo = null
  try {
    localStorage.removeItem(KEY)
  } catch {
    /* ignore */
  }
}

export function streamUrl(): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const token = getToken()
  const q = token ? `?token=${encodeURIComponent(token)}` : ''
  return `${proto}//${window.location.host}/v1/stream${q}`
}
