/**
 * Settings: everything that is configuration rather than activity.
 *
 * It exists because the chat panel was on the dashboard, and the dashboard is
 * for what is happening right now -- which agents need you, which are working.
 * An adapter's command and its environment are neither; they change once and
 * are then read only when something is wrong. Putting them above the grid cost
 * the top of the screen every time anyone opened the app to answer an agent.
 *
 * What it will NOT do is edit anything. Everything here is read from
 * config.toml and reported back, and the switches are shown so you can see
 * what they are, not toggled so you can change them from a browser. A page
 * that has been paired to by a phone over a LAN should not be able to enable a
 * chat bot or rewrite a command line that the daemon will execute.
 */

import { useStore } from '../store/store'
import type { ChatAdapterStatus } from '../protocol/types'

// `refused` is distinct from both neighbours on purpose: `misconfigured` is
// inferred from config, `restarting` means something is about to happen, and
// `refused` means the manager looked at this adapter and declined to spawn it.
const NEEDS_ATTENTION = new Set(['down', 'misconfigured', 'refused'])

function stateLabel(a: ChatAdapterStatus): string {
  switch (a.state) {
    case 'running':
      return 'running'
    case 'restarting':
      return a.restarts > 0 ? `restarting (${a.restarts} of ${a.max_restarts})` : 'starting'
    case 'down':
      return `gave up after ${a.restarts} restarts — not retrying`
    case 'misconfigured':
      return 'cannot run as configured'
    case 'refused':
      // Say that nothing is pending, because the neighbouring states both
      // imply something is.
      return 'refused — not started'
    case 'off':
      return !a.table_enabled ? 'off — [chat] is disabled' : 'off'
    default:
      return a.state
  }
}

export function Settings({ onBack }: { onBack: () => void }) {
  const chat = useStore((s) => s.welcome?.chat)
  const welcome = useStore((s) => s.welcome)
  const adapters = chat?.adapters ?? []

  return (
    <div className="settings">
      <header className="settings-head">
        <button type="button" className="settings-back" onClick={onBack}>
          ← Back
        </button>
        <h2>Settings</h2>
      </header>

      <section className="settings-section">
        <h3>
          Chat adapters
          {chat ? (
            <span className={`chat-pill${chat.enabled ? '' : ''}`}>
              {chat.enabled ? '[chat] enabled' : '[chat] disabled'}
            </span>
          ) : null}
        </h3>

        {!chat ? (
          <p className="chat-empty">This build has no chat support.</p>
        ) : adapters.length === 0 ? (
          <p className="chat-empty">
            No adapters configured.
            {chat.adapters_dir ? (
              <>
                {' '}
                Put one in <code>{chat.adapters_dir}</code> and name it under{' '}
                <code>[[chat.adapters]]</code> in <code>config.toml</code>.
              </>
            ) : null}
          </p>
        ) : (
          <ul className="chat-list">
            {adapters.map((a) => (
              <li key={a.id} className={`chat-row chat-${a.state}`}>
                <span className="chat-dot" aria-hidden="true" />
                <span className="chat-id">{a.id}</span>
                <span className="chat-state">{stateLabel(a)}</span>

                {a.command ? <code className="chat-cmd">{a.command}</code> : null}

                {a.env?.length ? (
                  <dl className="chat-env">
                    {a.env.map((e) => (
                      <div key={e.key} className="chat-env-row">
                        <dt>{e.key}</dt>
                        <dd>
                          <code>{e.literal}</code>
                          {e.reference ? (
                            <span className={e.resolved ? 'env-ok' : 'env-bad'}>
                              {e.resolved ? 'set' : 'not set'}
                              {chat.env_source === 'cli' ? ' in this shell' : ''}
                            </span>
                          ) : null}
                        </dd>
                      </div>
                    ))}
                  </dl>
                ) : null}

                {a.problems?.length ? (
                  <ul className="chat-problems">
                    {a.problems.map((p) => (
                      <li key={p}>{p}</li>
                    ))}
                  </ul>
                ) : null}

                {a.last_exit && a.state === 'down' ? (
                  <span className="chat-exit">last exit: {a.last_exit}</span>
                ) : null}
              </li>
            ))}
          </ul>
        )}

        {chat?.adapters_dir ? (
          <p className="settings-note">
            Adapters live in <code>{chat.adapters_dir}</code>. An adapter is a
            command — <code>node x.js</code>, <code>bun x.ts</code>,{' '}
            <code>go run x.go</code> — that reads messages on stdin and writes
            them on stdout. Edit <code>config.toml</code> to add or enable one;
            this page only reports.
          </p>
        ) : null}
      </section>

      <section className="settings-section">
        <h3>Server</h3>
        <dl className="chat-env">
          {welcome?.server_version ? (
            <div className="chat-env-row">
              <dt>version</dt>
              <dd>
                <code>{welcome.server_version}</code>
              </dd>
            </div>
          ) : null}
          {welcome?.herdr_version ? (
            <div className="chat-env-row">
              <dt>herdr</dt>
              <dd>
                <code>{welcome.herdr_version}</code>
              </dd>
            </div>
          ) : null}
        </dl>
      </section>
    </div>
  )
}

export function settingsNeedsAttention(adapters: ChatAdapterStatus[] | null | undefined): number {
  return (adapters ?? []).filter((a) => NEEDS_ATTENTION.has(a.state)).length
}

/**
 * The way in. It carries a dot when something needs a person, because the
 * whole reason configuration moved off the dashboard is that it is normally
 * boring -- and the one time it is not, nothing would otherwise say so.
 */
export function SettingsButton({
  attention,
  onClick,
}: {
  attention: number
  onClick: () => void
}) {
  return (
    <button
      type="button"
      className={`settings-btn${attention > 0 ? ' has-attention' : ''}`}
      onClick={onClick}
      aria-label={
        attention > 0
          ? `Settings — ${attention} adapter${attention === 1 ? '' : 's'} need attention`
          : 'Settings'
      }
      title="Settings"
    >
      <svg viewBox="0 0 16 16" aria-hidden="true">
        <circle cx="8" cy="8" r="2.4" fill="none" stroke="currentColor" strokeWidth="1.5" />
        <path
          d="M8 1.6v1.7M8 12.7v1.7M14.4 8h-1.7M3.3 8H1.6M12.5 3.5l-1.2 1.2M4.7 11.3l-1.2 1.2M12.5 12.5l-1.2-1.2M4.7 4.7L3.5 3.5"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
        />
      </svg>
      {attention > 0 ? <span className="settings-dot" aria-hidden="true" /> : null}
    </button>
  )
}
