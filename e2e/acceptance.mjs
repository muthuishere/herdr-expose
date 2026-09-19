/**
 * herdr-expose — the browser half of the real acceptance test.
 *
 * This is deliberately NOT a unit test and NOT a protocol probe. It drives a
 * real Chromium against a real `herdr-expose share --lan`, in front of a real
 * Herdr session holding a real agent, and asserts the things a human would
 * notice: that pairing works, that the tree is readable, that the terminal
 * shows what the agent said, that typing into the browser reaches the pane,
 * and — the one that matters most — that a pane left IDLE for a minute still
 * streams when it finally produces output.
 *
 * It is driven by e2e/acceptance.sh, which owns the throwaway session, the
 * share and the teardown. Run it through that; running it bare requires every
 * HEX_* variable below to already be true.
 *
 *   HEX_URL           base URL of the share            (http://ip:port)
 *   HEX_PAIR          a fresh single-use pairing code
 *   HEX_TOKEN         the token the agent was asked to reply with
 *   HEX_SESSION       herdr session name (shown in the sidebar)
 *   HEX_AGENT_PANE    pane id holding the agent        (e.g. w1:p1)
 *   HEX_SHELL_PANE    pane id holding a plain shell    (e.g. w1:p2)
 *   HERDR_SOCKET_PATH socket of that session, for read-back verification
 *   HEX_IDLE_SECS     idle soak before the LIVE test   (default 65)
 *   HEX_HEADED=1      watch it happen
 *   HEX_ARTIFACTS     screenshot directory             (default ./artifacts)
 *
 * Exit code is 0 only if every REQUIRED check passed.
 */

import { chromium } from 'playwright'
import { execFileSync, spawn } from 'node:child_process'
import { mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

const URL_BASE = req('HEX_URL')
const PAIR = req('HEX_PAIR')
const TOKEN = req('HEX_TOKEN')
const SESSION = req('HEX_SESSION')
const AGENT_PANE = process.env.HEX_AGENT_PANE || 'w1:p1'
const SHELL_PANE = process.env.HEX_SHELL_PANE || 'w1:p2'
const BLOCKED_PANE = process.env.HEX_BLOCKED_PANE || 'w1:p3'
const SHELL_TITLE = process.env.HEX_SHELL_TITLE || 'hexe2e-idle'
const BLOCKED_TITLE = process.env.HEX_BLOCKED_TITLE || 'hexe2e-blocked'
const IDLE_SECS = Number(process.env.HEX_IDLE_SECS || 65)
const ART = process.env.HEX_ARTIFACTS || join(process.cwd(), 'artifacts')
const HEADED = process.env.HEX_HEADED === '1'

function req(name) {
  const v = process.env[name]
  if (!v) {
    console.error(`missing ${name} — run this through e2e/acceptance.sh`)
    process.exit(2)
  }
  return v
}

/* ------------------------------------------------------------------ report */

const results = []
function check(name, ok, detail = '', required = true) {
  results.push({ name, ok: !!ok, detail, required })
  const mark = ok ? 'PASS' : required ? 'FAIL' : 'WARN'
  console.log(`[${mark}] ${name}${detail ? ` — ${detail}` : ''}`)
  return !!ok
}
function skip(name, why) {
  results.push({ name, ok: true, detail: `SKIPPED: ${why}`, required: false, skipped: true })
  console.log(`[SKIP] ${name} — ${why}`)
}

/* -------------------------------------------------------------- herdr side */

function herdr(...args) {
  return execFileSync('herdr', args, {
    env: { ...process.env, HERDR_SESSION: SESSION },
    encoding: 'utf8',
    maxBuffer: 8 * 1024 * 1024,
  })
}
function herdrJSON(...args) {
  return JSON.parse(herdr(...args)).result
}
/** Raw visible text of a pane, straight from Herdr — the ground truth. */
function paneText(pane, lines = 60) {
  try {
    return herdr('pane', 'read', pane, '--lines', String(lines))
  } catch (e) {
    return `<<read failed: ${e.message}>>`
  }
}
function agentState(pane) {
  try {
    return herdrJSON('agent', 'get', pane).agent?.agent_status ?? 'unknown'
  } catch {
    return 'unknown'
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

/* ------------------------------------------------------------ browser side */

/** Text the terminal is actually rendering. Requires the DOM renderer. */
async function termText(page) {
  return page.evaluate(() => {
    const rows = document.querySelector('.xterm-rows')
    return rows ? rows.innerText : ''
  })
}

/** Poll the rendered terminal until `needle` shows up. Returns ms, or -1. */
async function waitForTermText(page, needle, timeoutMs) {
  const t0 = Date.now()
  while (Date.now() - t0 < timeoutMs) {
    if ((await termText(page)).includes(needle)) return Date.now() - t0
    await sleep(250)
  }
  return -1
}

/**
 * Open a pane BY ID, not by title or position.
 *
 * Row order changes the moment something is pinned under "Needs you", and a
 * pane's title is whatever its shell last set, so both are unsafe handles in a
 * test. The app's own viewport map says which target it is streaming `live`,
 * which is exactly the question "is the right pane open?".
 */
async function openPane(page, session, paneId) {
  const target = `${session}/${paneId}`
  const rows = page.locator('.sidebar .row, .app-mobile .row')
  const n = await rows.count()
  for (let i = 0; i < n; i++) {
    // On the phone the list is REPLACED by the pane, so step back out before
    // trying the next candidate.
    const back = page.locator('.paneview .iconbtn[aria-label="Back to panes"]')
    if (await back.isVisible().catch(() => false)) {
      await back.click().catch(() => {})
      await page.locator('.row').first().waitFor({ state: 'visible', timeout: 5000 }).catch(() => {})
    }
    try {
      await rows.nth(i).click({ timeout: 10000 })
    } catch {
      continue
    }
    await page
      .locator('.term-host .xterm')
      .waitFor({ state: 'visible', timeout: 20000 })
      .catch(() => {})
    for (let j = 0; j < 12; j++) {
      const vp = (await stats(page))?.viewport ?? {}
      if (vp[target] === 'live') return true
      await sleep(250)
    }
  }
  return false
}

async function stats(page) {
  return page.evaluate(() => (window.__herdrStats ? window.__herdrStats() : null))
}

/**
 * Pixel difference between two PNG buffers, computed inside the page itself so
 * the harness needs no image dependency. Returns the fraction of differing
 * pixels (0..1), or -1 when the two shots are not the same size.
 */
async function pixelDiff(page, a, b) {
  const toURL = (buf) => 'data:image/png;base64,' + buf.toString('base64')
  return page.evaluate(
    async ([ua, ub]) => {
      const load = async (u) => createImageBitmap(await (await fetch(u)).blob())
      const [ia, ib] = await Promise.all([load(ua), load(ub)])
      if (ia.width !== ib.width || ia.height !== ib.height) return -1
      const draw = (img) => {
        const c = new OffscreenCanvas(img.width, img.height)
        const x = c.getContext('2d')
        x.drawImage(img, 0, 0)
        return x.getImageData(0, 0, img.width, img.height).data
      }
      const da = draw(ia)
      const db = draw(ib)
      let diff = 0
      for (let i = 0; i < da.length; i += 4) {
        if (da[i] !== db[i] || da[i + 1] !== db[i + 1] || da[i + 2] !== db[i + 2]) diff++
      }
      return diff / (ia.width * ia.height)
    },
    [toURL(a), toURL(b)],
  )
}

/* --------------------------------------------------------------------- run */

const main = async () => {
  mkdirSync(ART, { recursive: true })
  const browser = await chromium.launch({ headless: !HEADED })
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } })

  /**
   * Force xterm's DOM renderer before the app boots. WebGL and Canvas draw the
   * terminal into a <canvas>, which a test cannot read back as text. The app
   * already supports this fallback and persists it exactly here, so we are
   * using a shipped code path, not a hook added for the test.
   */
  await ctx.addInitScript(() => {
    try {
      const ua = navigator.userAgent.slice(0, 200)
      const at = Date.now()
      localStorage.setItem(
        'herdr-expose.renderer-blocklist',
        JSON.stringify([
          { ua, kind: 'webgl', at },
          { ua, kind: 'canvas', at },
        ]),
      )
    } catch {}
  })

  const page = await ctx.newPage()
  const consoleErrors = []
  page.on('console', (m) => {
    if (m.type() === 'error') consoleErrors.push(m.text())
  })
  page.on('pageerror', (e) => consoleErrors.push(String(e)))

  const shot = async (name, target) =>
    (target ?? page).screenshot({ path: join(ART, `${name}.png`) })

  /* -- 1. unpaired browsers get the pairing screen ----------------------- */
  await page.goto(URL_BASE, { waitUntil: 'domcontentloaded' })
  const pairVisible = await page
    .locator('.pair-input')
    .waitFor({ state: 'visible', timeout: 15000 })
    .then(() => true)
    .catch(() => false)
  await shot('01-pair-screen')
  check('unpaired device is stopped at the pairing screen', pairVisible)

  /* -- 2. the ?pair= deep link gets past it ------------------------------ */
  await page.goto(`${URL_BASE}/?pair=${PAIR}`, { waitUntil: 'domcontentloaded' })
  const paired = await page
    .locator('.panelist')
    .waitFor({ state: 'visible', timeout: 20000 })
    .then(() => true)
    .catch(() => false)
  check('the ?pair= deep link pairs the device', paired)
  const urlAfter = page.url()
  check(
    'the pairing code is scrubbed from the URL',
    !urlAfter.includes('pair='),
    urlAfter,
    false,
  )
  await shot('02-paired-tree')

  if (!paired) {
    await browser.close()
    return finish()
  }

  /* -- 3. the tree is human-readable ------------------------------------- */
  const sidebarText = await page.locator('.sidebar').innerText()
  const sessionNames = await page.locator('.session-name').allInnerTexts()
  const rowTitles = await page.locator('.sidebar .row-title').allInnerTexts()
  check(
    'the session appears in the sidebar under its own name',
    // innerText is the CSS-transformed text and the header is uppercased.
    sessionNames.some((n) => n.toLowerCase() === SESSION.toLowerCase()),
    `saw [${sessionNames.join(', ')}]`,
  )
  check(
    'the panes appear with human titles',
    rowTitles.length >= 2 && rowTitles.every((t) => t.trim().length > 0),
    `${rowTitles.length} rows: ${JSON.stringify(rowTitles)}`,
  )
  const rawIds = sidebarText.match(/\bw\d+:[tp]\d+\b/g) || []
  check('no raw pane/tab ids are rendered as labels', rawIds.length === 0, rawIds.join(' '))
  // A pane the user has NAMED should win over whatever its shell last set as
  // the terminal title. herdr-expose has no `label` field at all today.
  check(
    'a user-set pane name (`herdr pane rename`) reaches the browser',
    rowTitles.some((t) => t.includes(SHELL_TITLE)),
    `looked for "${SHELL_TITLE}" in ${JSON.stringify(rowTitles)}`,
    false,
  )

  /* -- 4. the terminal renders what the agent says ----------------------- */
  // Open the pane FIRST and let the geometry settle: attaching resizes the
  // PTY, and a full-screen TUI redraws (and loses its old screen) on SIGWINCH.
  // Then make the agent speak and watch the answer arrive live.
  check(
    `the agent pane (${AGENT_PANE}) opens and goes live`,
    await openPane(page, SESSION, AGENT_PANE),
  )
  await sleep(2500)
  const promptProc = spawn(
    'herdr',
    ['agent', 'prompt', AGENT_PANE, `Reply with exactly this text and nothing else: ${TOKEN}`],
    { env: { ...process.env, HERDR_SESSION: SESSION }, stdio: 'ignore' },
  )
  const tokenMs = await waitForTermText(page, TOKEN, 120000)
  promptProc.kill('SIGTERM')
  await shot('03-terminal-with-token')
  check(
    `the terminal renders the agent's reply (${TOKEN}) as it is produced`,
    tokenMs >= 0,
    tokenMs >= 0 ? `visible after ${tokenMs}ms` : 'never appeared in 120s',
  )
  check(
    'and Herdr agrees the pane really contains it',
    paneText(AGENT_PANE).includes(TOKEN),
    'cross-checked with `herdr pane read`',
  )

  /* -- 5. keystrokes typed in the browser reach the pane ----------------- */
  const typed = `hexe2e-typed-${Math.random().toString(16).slice(2, 8)}`
  await page.locator('.term-host').click()
  await page.keyboard.type(typed, { delay: 25 })
  let arrived = -1
  for (let i = 0; i < 40; i++) {
    if (paneText(AGENT_PANE).includes(typed)) {
      arrived = i * 250
      break
    }
    await sleep(250)
  }
  await shot('04-typed-from-browser')
  check(
    'typing in the browser reaches the pane (verified with `herdr pane read`)',
    arrived >= 0,
    arrived >= 0 ? `"${typed}" landed in ${AGENT_PANE} within ~${arrived}ms` : `"${typed}" never arrived`,
  )
  // Leave the agent's prompt box as we found it.
  await page.keyboard.press('Control+u').catch(() => {})

  /* -- 6. the agent badge reflects reality ------------------------------- */
  // Both sides move: wait for agreement rather than sampling a race. A badge
  // that never catches up inside 20s is the bug this is looking for.
  let badgeState = null
  let truth = null
  const t0badge = Date.now()
  while (Date.now() - t0badge < 20000) {
    truth = agentState(AGENT_PANE)
    badgeState = await page
      .locator('.paneview .badge')
      .first()
      .getAttribute('data-state')
      .catch(() => null)
    if (badgeState === truth) break
    await sleep(500)
  }
  check(
    'the agent state badge matches what Herdr reports',
    badgeState === truth,
    `browser=${badgeState} herdr=${truth} after ${Date.now() - t0badge}ms`,
  )
  // Watch it move: drive the agent and require the badge to report `working`.
  const busy = spawn('herdr', ['agent', 'prompt', AGENT_PANE, 'Count slowly from 1 to 20, one number per line.'],
    { env: { ...process.env, HERDR_SESSION: SESSION }, stdio: 'ignore' })
  let sawWorking = false
  for (let i = 0; i < 60; i++) {
    const b = await page.locator('.paneview .badge').first().getAttribute('data-state').catch(() => null)
    if (b === 'working') { sawWorking = true; break }
    await sleep(500)
  }
  check('the badge goes to `working` while the agent works', sawWorking)
  busy.kill('SIGTERM')

  /* -- 7. a blocked agent shows its question ----------------------------- */
  // Driven through `herdr pane report-agent`, the documented API an agent uses
  // to declare its own lifecycle state. Baiting a real coding agent into an
  // approval dialog is not reproducible (it depends on that agent's permission
  // settings); what herdr-expose owes us is that when Herdr SAYS blocked, the
  // browser pins the pane, flips the badge and shows the question.
  const QUESTION = 'Do you want to proceed with the e2e acceptance check?'
  let blockedSeen = false
  try {
    herdr(
      'pane', 'report-agent', BLOCKED_PANE,
      '--source', 'hexe2e-test', '--agent', 'claude', '--state', 'blocked',
      '--message', QUESTION,
    )
    for (let i = 0; i < 20; i++) {
      if (agentState(BLOCKED_PANE) === 'blocked') {
        blockedSeen = true
        break
      }
      await sleep(500)
    }
  } catch (e) {
    console.log(`  (could not report a blocked state: ${e.message})`)
  }
  if (blockedSeen) {
    // Exactly one pane is blocked, so the urgent section has exactly one row.
    const urgentRow = page.locator('.ws-urgent .row').first()
    const pinned = await urgentRow
      .waitFor({ state: 'visible', timeout: 15000 })
      .then(() => true)
      .catch(() => false)
    check('a blocked pane is pinned under "Needs you"', pinned)
    if (pinned) await urgentRow.click()
    const qa = page.locator('.qa')
    const shown = await qa
      .waitFor({ state: 'visible', timeout: 15000 })
      .then(() => true)
      .catch(() => false)
    const qaText = shown ? await qa.innerText() : ''
    await shot('05-blocked-question')
    // The panel renders the pane's own detection text verbatim, which is the
    // right thing: a question is whatever the agent actually printed.
    check(
      'a blocked agent shows its question in the browser',
      shown && qaText.replace(/needs an answer/i, '').trim().length > 0,
      shown ? JSON.stringify(qaText.slice(0, 100)) : 'no .qa panel appeared',
    )
    const badge2 = await page.locator('.paneview .badge').first().getAttribute('data-state')
    check('the badge flips to blocked', badge2 === 'blocked', `badge=${badge2}`)
    const keys = await page.locator('.keybar .key-quick').allInnerTexts()
    check(
      'the answer key bar is offered (y / n / 1 / 2 / 3)',
      ['y', 'n', '1', '2', '3'].every((k) => keys.includes(k)),
      JSON.stringify(keys),
      false,
    )
    herdr('pane', 'report-agent', BLOCKED_PANE,
      '--source', 'hexe2e-test', '--agent', 'claude', '--state', 'idle')
  } else {
    skip('a blocked agent shows its question', 'Herdr never reported the pane as blocked')
  }

  /* -- 8. THE ONE THAT MATTERS: an idle pane still streams --------------- */
  // A plain shell, not the agent: an agent TUI repaints on its own and would
  // mask exactly the bug this is here to catch. A bash prompt emits nothing.
  const shellOpen = await openPane(page, SESSION, SHELL_PANE)
  check(`the idle pane (${SHELL_PANE}) opens and goes live`, shellOpen)
  await sleep(2000)

  const statsBefore = await stats(page)
  const shotA = await page.locator('.term-wrap').screenshot()
  await sleep(2000)
  const shotB = await page.locator('.term-wrap').screenshot()
  const diff = await pixelDiff(page, shotA, shotB)
  writeFileSync(join(ART, '06-settled-a.png'), shotA)
  writeFileSync(join(ART, '06-settled-b.png'), shotB)
  check(
    'a settled terminal does not flicker (2s apart, pixel diff)',
    diff >= 0 && diff < 0.02,
    diff < 0 ? 'screenshots differ in size' : `${(diff * 100).toFixed(3)}% of pixels changed`,
  )

  console.log(`  ... holding an IDLE pane open for ${IDLE_SECS}s ...`)
  await sleep(IDLE_SECS * 1000)

  const statsMid = await stats(page)
  const tgtBefore = statsBefore?.targets?.[`${SESSION}/${SHELL_PANE}`] ?? {}
  const tgtMid = statsMid?.targets?.[`${SESSION}/${SHELL_PANE}`] ?? {}
  check(
    `no terminal churn over the ${IDLE_SECS}s idle window (reset/termCreate stay put)`,
    (tgtMid.reset ?? 0) === (tgtBefore.reset ?? 0) &&
      (tgtMid.termCreate ?? 0) === (tgtBefore.termCreate ?? 0),
    `reset ${tgtBefore.reset ?? 0}->${tgtMid.reset ?? 0}, termCreate ${tgtBefore.termCreate ?? 0}->${tgtMid.termCreate ?? 0}`,
  )
  check(
    'termCreate is exactly 1 for this pane',
    (tgtMid.termCreate ?? 0) === 1,
    `termCreate=${tgtMid.termCreate ?? 0}`,
  )

  const token2 = `HEXE2E-LIVE-${Math.random().toString(16).slice(2, 8)}`
  herdr('pane', 'send-text', SHELL_PANE, `echo ${token2}`)
  await sleep(200)
  herdr('pane', 'send-keys', SHELL_PANE, 'enter')
  const liveMs = await waitForTermText(page, token2, 30000)
  await shot('07-idle-then-output')
  check(
    `output produced after ${IDLE_SECS}s of idle still reaches the browser`,
    liveMs >= 0,
    liveMs >= 0
      ? `appeared after ${liveMs}ms`
      : `${token2} NEVER appeared in 30s — the LIVE stream died on the idle pane`,
  )
  if (liveMs < 0) {
    writeFileSync(
      join(ART, 'idle-bug-evidence.txt'),
      [
        `token: ${token2}`,
        `pane:  ${SHELL_PANE}`,
        '--- what the pane really contains, per herdr ---',
        paneText(SHELL_PANE),
        '--- what the browser is rendering ---',
        await termText(page),
        '--- stats ---',
        JSON.stringify(await stats(page), null, 2),
      ].join('\n'),
    )
  }

  const statsAfter = await stats(page)
  writeFileSync(join(ART, 'stats.json'), JSON.stringify(statsAfter, null, 2))

  /* -- 9. 375px, the design target --------------------------------------- */
  await page.setViewportSize({ width: 375, height: 812 })
  await page.reload({ waitUntil: 'domcontentloaded' })
  const mobileShell = await page
    .locator('.app-mobile')
    .waitFor({ state: 'visible', timeout: 15000 })
    .then(() => true)
    .catch(() => false)
  check('375px gets the mobile shell, not a shrunken desktop', mobileShell)
  const mobileRows = await page.locator('.row-title').allInnerTexts()
  check('the pane list is usable at 375px', mobileRows.length >= 2, `${mobileRows.length} rows`)
  await shot('08-mobile-list')
  const mobileOpen = await openPane(page, SESSION, SHELL_PANE)
  check('the idle pane opens at 375px', mobileOpen)
  const mobileTerm = await page
    .locator('.term-host .xterm')
    .waitFor({ state: 'visible', timeout: 20000 })
    .then(() => true)
    .catch(() => false)
  await shot('09-mobile-terminal')
  check('the terminal renders at 375px', mobileTerm)
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - window.innerWidth,
  )
  check('nothing overflows horizontally at 375px', overflow <= 1, `${overflow}px of overflow`)

  check(
    'no uncaught console errors',
    consoleErrors.length === 0,
    consoleErrors.slice(0, 3).join(' | '),
    false,
  )

  await browser.close()
  finish()
}

function finish() {
  const failed = results.filter((r) => r.required && !r.ok)
  console.log('\n' + '='.repeat(72))
  console.log(
    `${results.filter((r) => r.ok && !r.skipped).length} passed, ${failed.length} failed, ` +
      `${results.filter((r) => r.skipped).length} skipped`,
  )
  if (failed.length) {
    console.log('\nFAILED:')
    for (const f of failed) console.log(`  - ${f.name}${f.detail ? ` (${f.detail})` : ''}`)
  }
  console.log(`artifacts: ${ART}`)
  writeFileSync(join(ART, 'results.json'), JSON.stringify(results, null, 2))
  process.exit(failed.length ? 1 : 0)
}

main().catch((e) => {
  console.error('harness crashed:', e)
  check('harness completed', false, String(e))
  finish()
})
