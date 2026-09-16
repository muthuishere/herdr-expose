/**
 * Environment facts the UI must state plainly rather than fail silently about.
 *
 * SPEC AMENDMENTS 7 adds a LAN mode that binds 0.0.0.0 when cloudflared is
 * absent. A LAN IP over plain HTTP is NOT a secure context: service workers and
 * PWA install are unavailable there. `localhost`/`127.0.0.1` are exempt by
 * spec; `192.168.x.x` is not.
 *
 * The app still works fully over LAN — it is a live WebSocket, which plain HTTP
 * permits. What is lost is offline shell + "Add to Home Screen". Saying so is
 * better than an install button that does nothing.
 */

export type ReachMode = 'localhost' | 'lan' | 'secure'

export interface EnvStatus {
  mode: ReachMode
  /** Service worker + install are only possible in a secure context. */
  pwaAvailable: boolean
  /** One sentence, ready to render. Empty when there is nothing to say. */
  note: string
}

export function envStatus(): EnvStatus {
  const { protocol, hostname } = window.location
  const isLoopback =
    hostname === 'localhost' ||
    hostname === '127.0.0.1' ||
    hostname === '::1' ||
    hostname === '[::1]'
  const secure = window.isSecureContext === true

  if (protocol === 'https:' || (secure && !isLoopback)) {
    return { mode: 'secure', pwaAvailable: true, note: '' }
  }
  if (isLoopback) {
    // Loopback is a secure context by spec, so the PWA works here — but this is
    // the machine itself, where you would use the terminal directly anyway.
    return { mode: 'localhost', pwaAvailable: secure, note: '' }
  }
  return {
    mode: 'lan',
    pwaAvailable: false,
    note:
      'On a LAN address over plain HTTP, browsers block service workers and ' +
      'Add to Home Screen — that needs HTTPS. Everything else works. Expose it ' +
      'through the Cloudflare tunnel to install it as an app.',
  }
}

/** True when the browser will even consider an install prompt. */
export function canInstall(): boolean {
  return envStatus().pwaAvailable && 'serviceWorker' in navigator
}
