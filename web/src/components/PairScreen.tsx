/**
 * Pairing screen. Mobile-first: this is a phone screen first and a desktop
 * screen only incidentally, because pairing only ever happens on a phone.
 *
 * Two entry paths, one code path:
 *   1. The QR on the local machine encodes `https://<host>/?pair=<code>`. The
 *      phone lands here with the code already in hand and we redeem it
 *      automatically — the user taps nothing.
 *   2. Someone reads the code aloud and types the six characters.
 */

import { useCallback, useEffect, useRef, useState } from 'react'
import { normalizeCode, redeemPairCode } from '../net/pair'

export function PairScreen({
  initialCode,
  onPaired,
}: {
  initialCode?: string | null
  onPaired: () => void
}) {
  const [code, setCode] = useState(initialCode ?? '')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)
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
      </div>
    </div>
  )
}
