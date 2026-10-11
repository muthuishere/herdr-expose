/**
 * Password login — `POST /v1/login` (ADR 0039).
 *
 * The SECOND credential, and the weaker one. A pairing code is minted on the
 * machine and shown only on its screen, which is what makes it strong and also
 * what makes it useless when you are not standing there: if the hostname in
 * front of this server changes, every stored device token stops matching its
 * origin and the only way back in is to walk to the machine.
 *
 * That is the hole this fills. The password makes RE-AUTHENTICATION something
 * you can do from wherever you are, without weakening origin binding at all —
 * tokens stay origin-bound, it is recovery that stops needing your feet.
 *
 * It answers in exactly the shape `/v1/pair` does, so this file is deliberately
 * the same shape as pair.ts and the screen can treat the two as one flow.
 *
 * WHY THIS IS ALWAYS OFFERED, even when no password is set: the server refuses
 * a wrong password, an unset password and a rate-limited attempt with one
 * identical opaque 401, and `/v1/config` does not advertise whether a password
 * exists. Hiding the field when none is configured would rebuild the oracle the
 * server went to the trouble of not having — a prober could tell a locked-down
 * instance from one with the feature off by looking at the login screen.
 */

import { setToken } from './auth'

export interface LoginResult {
  ok: boolean
  error?: string
}

/**
 * A label for this device, shown in `herdr-expose devices`. It is a
 * convenience, not an identity: the server neither trusts nor verifies it.
 */
function deviceName(): string {
  const ua = navigator.userAgent
  for (const [re, name] of [
    [/iPhone/i, 'iPhone'],
    [/iPad/i, 'iPad'],
    [/Android/i, 'Android'],
    [/Macintosh/i, 'Mac'],
    [/Windows/i, 'Windows'],
    [/Linux/i, 'Linux'],
  ] as const) {
    if (re.test(ua)) return name
  }
  return 'device'
}

export async function loginWithPassword(raw: string): Promise<LoginResult> {
  // TRIMMED, to match how the password was stored. `config set-password` reads
  // through readSecret, which TrimSpace-es its input, so a password with
  // surrounding whitespace cannot exist on the server side — and a phone
  // keyboard's autocorrect will happily append a space after a pasted value.
  // Not trimming here would turn a correct password into an unexplainable
  // failure, which is the worst outcome for a credential whose only error
  // message is deliberately opaque.
  const password = raw.trim()
  // Refused locally, because sending it would spend one of a deliberately
  // small number of attempts against the rate limiter.
  if (password === '') return { ok: false, error: 'Enter the password.' }
  try {
    const res = await fetch('/v1/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password, name: deviceName() }),
      credentials: 'omit',
    })
    if (!res.ok) {
      return {
        ok: false,
        error:
          res.status === 429
            ? 'Too many attempts. Wait a moment and try again.'
            : // The server's 401 is deliberately opaque and we must not dress
              // it up: "wrong password" would be a claim this client cannot
              // support, since the same answer covers an unset password and a
              // rate limit that has already been hit.
              'That was not accepted. Check the password, or pair with a code instead.',
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
