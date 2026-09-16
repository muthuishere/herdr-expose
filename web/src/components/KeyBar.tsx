/**
 * The fixed key bar — SPEC B8 / §8.
 *
 * Order is deliberate: ESC TAB CTRL ALT arrows, then the drawers (ctrl-chords,
 * symbols, F-keys), and **Enter LAST and rightmost**, under the thumb.
 *
 * Modifiers are LATCHES, not held keys: tap CTRL, then tap C. A second tap
 * un-latches; a long-press locks until tapped again.
 *
 * It sits above the iOS home indicator via env(safe-area-inset-bottom) and
 * lifts by --keyboard-inset when the soft keyboard is open.
 */

import { useCallback, useState } from 'react'
import type { PaneId } from '../protocol/types'
import { sendInputText } from '../net/connection'

type Drawer = null | 'ctrl' | 'sym' | 'fn'

export interface KeyBarProps {
  target: PaneId
  /** Extra keys pinned in front, used by the blocked-agent Q&A view. */
  quickKeys?: Array<{ label: string; seq: string; tone?: 'yes' | 'no' }>
}

const ESC = '\x1b'

/** ctrl+<letter> is the letter's code AND 0x1f. */
function ctrlSeq(ch: string): string {
  const c = ch.toUpperCase().charCodeAt(0)
  if (c >= 64 && c <= 95) return String.fromCharCode(c & 0x1f)
  return ch
}

const CTRL_CHORDS = ['a', 'b', 'c', 'd', 'e', 'k', 'l', 'n', 'p', 'r', 'u', 'w', 'z']
const SYMBOLS = ['|', '~', '/', '\\', '-', '_', '*', '#', '$', '%', '^', '&', '{', '}', '[', ']']
const FKEYS: Array<[string, string]> = [
  ['F1', `${ESC}OP`],
  ['F2', `${ESC}OQ`],
  ['F3', `${ESC}OR`],
  ['F4', `${ESC}OS`],
  ['F5', `${ESC}[15~`],
  ['F6', `${ESC}[17~`],
  ['F7', `${ESC}[18~`],
  ['F8', `${ESC}[19~`],
  ['F9', `${ESC}[20~`],
  ['F10', `${ESC}[21~`],
  ['F11', `${ESC}[23~`],
  ['F12', `${ESC}[24~`],
]

export function KeyBar({ target, quickKeys }: KeyBarProps) {
  const [ctrl, setCtrl] = useState<'off' | 'latched' | 'locked'>('off')
  const [alt, setAlt] = useState<'off' | 'latched' | 'locked'>('off')
  const [drawer, setDrawer] = useState<Drawer>(null)

  const send = useCallback(
    (raw: string, isPrintable: boolean) => {
      let out = raw
      if (isPrintable && ctrl !== 'off') out = ctrlSeq(out)
      if (alt !== 'off') out = ESC + out
      sendInputText(target, out)
      if (ctrl === 'latched') setCtrl('off')
      if (alt === 'latched') setAlt('off')
    },
    [target, ctrl, alt],
  )

  const toggle = (
    v: 'off' | 'latched' | 'locked',
    set: (s: 'off' | 'latched' | 'locked') => void,
  ) => set(v === 'off' ? 'latched' : 'off')

  const lock = (
    v: 'off' | 'latched' | 'locked',
    set: (s: 'off' | 'latched' | 'locked') => void,
  ) => set(v === 'locked' ? 'off' : 'locked')

  const openDrawer = (d: Drawer) => setDrawer((cur) => (cur === d ? null : d))

  return (
    <div className="keybar" role="toolbar" aria-label="Terminal keys">
      {drawer && (
        <div className="keybar-drawer" role="group">
          {drawer === 'ctrl' &&
            CTRL_CHORDS.map((c) => (
              <button
                key={c}
                className="key key-sm"
                onPointerDown={press(() => sendInputText(target, ctrlSeq(c)))}
              >
                ^{c.toUpperCase()}
              </button>
            ))}
          {drawer === 'sym' &&
            SYMBOLS.map((c) => (
              <button
                key={c}
                className="key key-sm"
                onPointerDown={press(() => send(c, true))}
              >
                {c}
              </button>
            ))}
          {drawer === 'fn' &&
            FKEYS.map(([label, seq]) => (
              <button
                key={label}
                className="key key-sm"
                onPointerDown={press(() => sendInputText(target, seq))}
              >
                {label}
              </button>
            ))}
        </div>
      )}

      <div className="keybar-row">
        <div className="keybar-scroll">
        {quickKeys?.map((k) => (
          <button
            key={k.label}
            className={`key key-quick${k.tone ? ` key-${k.tone}` : ''}`}
            onPointerDown={press(() => sendInputText(target, k.seq))}
          >
            {k.label}
          </button>
        ))}

        <button className="key" onPointerDown={press(() => sendInputText(target, ESC))}>
          esc
        </button>
        <button className="key" onPointerDown={press(() => sendInputText(target, '\t'))}>
          tab
        </button>
        <button
          className={`key key-mod${ctrl !== 'off' ? ' is-on' : ''}${ctrl === 'locked' ? ' is-locked' : ''}`}
          aria-pressed={ctrl !== 'off'}
          onPointerDown={press(() => toggle(ctrl, setCtrl))}
          onContextMenu={(e) => {
            e.preventDefault()
            lock(ctrl, setCtrl)
          }}
        >
          ctrl
        </button>
        <button
          className={`key key-mod${alt !== 'off' ? ' is-on' : ''}${alt === 'locked' ? ' is-locked' : ''}`}
          aria-pressed={alt !== 'off'}
          onPointerDown={press(() => toggle(alt, setAlt))}
          onContextMenu={(e) => {
            e.preventDefault()
            lock(alt, setAlt)
          }}
        >
          alt
        </button>

        <button
          className="key key-arrow"
          aria-label="Up"
          onPointerDown={press(() => sendInputText(target, `${ESC}[A`))}
        >
          ↑
        </button>
        <button
          className="key key-arrow"
          aria-label="Down"
          onPointerDown={press(() => sendInputText(target, `${ESC}[B`))}
        >
          ↓
        </button>
        <button
          className="key key-arrow"
          aria-label="Left"
          onPointerDown={press(() => sendInputText(target, `${ESC}[D`))}
        >
          ←
        </button>
        <button
          className="key key-arrow"
          aria-label="Right"
          onPointerDown={press(() => sendInputText(target, `${ESC}[C`))}
        >
          →
        </button>

        <button
          className={`key key-drawer${drawer === 'ctrl' ? ' is-on' : ''}`}
          onPointerDown={press(() => openDrawer('ctrl'))}
        >
          ^
        </button>
        <button
          className={`key key-drawer${drawer === 'sym' ? ' is-on' : ''}`}
          onPointerDown={press(() => openDrawer('sym'))}
        >
          #
        </button>
        <button
          className={`key key-drawer${drawer === 'fn' ? ' is-on' : ''}`}
          onPointerDown={press(() => openDrawer('fn'))}
        >
          fn
        </button>
        </div>

        {/* Enter LAST and rightmost — under the thumb (B8). It sits OUTSIDE the
            scroller so it is always reachable and never overlaps a key. */}
        <button
          className="key key-enter"
          onPointerDown={press(() => sendInputText(target, '\r'))}
        >
          ⏎
        </button>
      </div>
    </div>
  )
}

/**
 * pointerdown, not click: it fires ~100ms sooner and never waits on a
 * double-tap timer. preventDefault keeps focus off the button so the soft
 * keyboard state does not flicker.
 */
function press(fn: () => void) {
  return (e: React.PointerEvent) => {
    e.preventDefault()
    e.stopPropagation()
    fn()
  }
}
