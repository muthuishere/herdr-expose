/**
 * Pairing screen. Mobile-first: this is a phone screen first and a desktop
 * screen only incidentally, because pairing only ever happens on a phone.
 *
 * THREE entry paths, two credentials:
 *   1. The QR on the local machine encodes `https://<host>/?pair=<code>`. The
 *      phone lands here with the code already in hand and we redeem it
 *      automatically — the user taps nothing.
 *   2. Someone reads the code aloud and types the six characters.
 *   3. The login password (ADR 0039), for when nobody is standing at the
 *      machine to read a code off it.
 *
 * Why (3) exists at all: a pairing code is minted locally and displayed only
 * on that screen, which is exactly what makes it strong — and exactly what
 * makes it unusable from a train. Device tokens are origin-bound, so the day
 * the hostname in front of this server changes, every paired device is logged
 * out and the only documented way back in was to walk to the machine. The
 * password makes recovery remote without loosening origin binding.
 *
 * The password field is shown UNCONDITIONALLY, even when no password is set.
 * The server answers a wrong password, an unset password and a rate-limited
 * attempt with one identical 401, and `/v1/config` deliberately does not say
 * whether a password exists. Hiding the field when none is configured would
 * rebuild that oracle in the UI — the login screen itself would tell a prober
 * which instances have the feature off.
 */

import { useCallback, useEffect, useRef, useState } from 'react'
import { normalizeCode, redeemPairCode } from '../net/pair'
import { loginWithPassword } from '../net/login'

export function PairScreen({
  initialCode,
  notice,
  onPaired,
}: {
  initialCode?: string | null
  /**
   * Why we are here. "never paired" and "your device was revoked" are
   * different events and the second one must not look like the first.
   */
  notice?: string | null
  onPaired: () => void
}) {
  const [code, setCode] = useState(initialCode ?? '')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const pwRef = useRef<HTMLInputElement>(null)
  const autoTried = useRef(false)

  const submit = useCallback(
    async (raw: string) => {
      setBusy(true)
      setError(null)
      const res = await redeemPairCode(raw)
      setBusy(false)
      if (res.ok) onPaired()
      else {
        setError(res.error ?? 'Pairing failed.')
        setCode('')
        inputRef.current?.focus()
      }
    },
    [onPaired],
  )

  const login = useCallback(async () => {
    setBusy(true)
    setError(null)
    const res = await loginWithPassword(password)
    setBusy(false)
    if (res.ok) {
      onPaired()
      return
    }
    setError(res.error ?? 'Login failed.')
    // The password is CLEARED on failure, unlike a pairing code, which is
    // cleared because it is single-use and already spent. Here the reason is
    // different: a wrong password left sitting in a focused field on a phone
    // is a credential on a screen somebody else can see over your shoulder.
    setPassword('')
    pwRef.current?.focus()
  }, [password, onPaired])

  // A code from the QR deep link redeems itself: the phone should just work.
  useEffect(() => {
    if (autoTried.current) return
    if (initialCode && normalizeCode(initialCode).length === 6) {
      autoTried.current = true
      void submit(initialCode)
    }
  }, [initialCode, submit])

  return (
    <div className="pair">
      <div className="pair-card">
        <h1 className="pair-title">Pair this device</h1>
        {notice ? (
          <p className="pair-notice" role="status">
            {notice}
          </p>
        ) : null}
        <p className="pair-sub">
          Run <code>herdr-expose pair</code> on your machine and scan the QR, or type the
          six-character code here.
        </p>

        <form
          className="pair-form"
          onSubmit={(e) => {
            e.preventDefault()
            if (!busy) void submit(code)
          }}
        >
          <input
            ref={inputRef}
            className="pair-input"
            value={code}
            onChange={(e) => setCode(normalizeCode(e.target.value))}
            placeholder="ABC123"
            inputMode="text"
            autoCapitalize="characters"
            autoCorrect="off"
            autoComplete="one-time-code"
            spellCheck={false}
            maxLength={6}
            aria-label="Pairing code"
            disabled={busy}
          />
          <button
            className="pair-submit"
            type="submit"
            disabled={busy || normalizeCode(code).length !== 6}
          >
            {busy ? 'Pairing…' : 'Pair'}
          </button>
        </form>

        {error ? (
          <p className="pair-error" role="alert">
            {error}
          </p>
        ) : (
          <p className="pair-hint">Codes expire after 10 minutes and work once.</p>
        )}

        <div className="pair-or">
          <span>or</span>
        </div>

        <form
          className="pair-form"
          onSubmit={(e) => {
            e.preventDefault()
            if (!busy) void login()
          }}
        >
          <input
            ref={pwRef}
            className="pair-input is-password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="Login password"
            // current-password, not new-password: a password manager should
            // offer to FILL this, never to generate one. The password is set on
            // the machine with `herdr-expose config set-password`; this screen
            // can only ever spend it.
            autoComplete="current-password"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            aria-label="Login password"
            disabled={busy}
          />
          <button className="pair-submit" type="submit" disabled={busy || password.trim() === ""}>
            {busy ? 'Signing in…' : 'Log in'}
          </button>
        </form>

        <p className="pair-hint">
          Set one on the machine with <code>herdr-expose config set-password</code>, or{' '}
          <code>config generate-password</code> for a strong one. Use this when nobody is at
          the machine to read a code off it.
        </p>
      </div>
    </div>
  )
}
