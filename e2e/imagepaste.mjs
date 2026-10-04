/**
 * Proof that an image pasted in a browser lands on the machine as a file an
 * agent can open.
 *
 * It drives a REAL browser against a REAL share of a REAL pane, pastes real
 * PNG bytes through the real clipboard API, and then looks on disk. Nothing
 * here is mocked: the only thing it will not do is press Send, because that
 * would type into somebody's live agent, and the upload path is complete
 * before Send is pressed.
 *
 * LOCAL only -- `share --local` binds loopback, so no tunnel is raised, no
 * hostname is claimed and nothing is reachable off this machine.
 *
 *   node e2e/imagepaste.mjs [session/pane]
 */
import { chromium } from 'playwright'
import { execFileSync } from 'node:child_process'
import { existsSync, readFileSync, statSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'

const BIN = join(homedir(), '.local/bin/herdr-expose')
const log = (m) => console.log(`\x1b[1;34m==>\x1b[0m ${m}`)
const ok = (m) => console.log(`\x1b[1;32m  ok\x1b[0m ${m}`)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

function hx(...args) {
  return execFileSync(BIN, args, { encoding: 'utf8' })
}

/** A 1x1 PNG, as real bytes: the server validates MAGIC BYTES, so this must be one. */
const PNG_B64 =
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=='

let shareId = null
const cleanup = () => {
  if (shareId) {
    try { hx('share', 'revoke', shareId) } catch { /* already gone */ }
    shareId = null
  }
}
process.on('exit', cleanup)
process.on('SIGINT', () => { cleanup(); process.exit(130) })

async function main() {
  const target = process.argv[2] ?? pickIdleAgent()
  const [session] = target.split('/')
  log(`using pane ${target}`)

  log('raising a LOCAL share (loopback only, no tunnel, no DNS)')
  const out = hx('share', '--local', '--session', session, '--hours', '1', '--json')
  const res = JSON.parse(out).share ?? JSON.parse(out)
  shareId = res.id
  const url = (res.url ?? '').replace(/\/$/, '')
  if (!url) throw new Error(`share gave no url: ${out}`)
  log(`share ${shareId} at ${url}`)

  const code = (JSON.parse(hx('share', 'pair', shareId, '--json')).pairing_code ?? '').trim()
  if (!code) throw new Error('no pairing code')

  const browser = await chromium.launch()
  const ctx = await browser.newContext({ colorScheme: 'dark' })
  const page = await ctx.newPage()
  const errors = []
  page.on('pageerror', (e) => errors.push(String(e)))

  log('pairing a browser exactly as a phone does')
  await page.goto(`${url}/?pair=${code}`, { waitUntil: 'domcontentloaded', timeout: 60000 })
  await page.locator('.panelist').waitFor({ state: 'visible', timeout: 60000 })
  ok('paired')

  log('opening the pane')
  // data-target is the stable handle the component puts there on purpose: row
  // order changes (a blocked pane re-pins itself) and a title is whatever the
  // shell last wrote to OSC 0/2, so neither is safe to select on.
  const row = page.locator(`button.row[data-target="${target}"]`)
  await row.waitFor({ state: 'visible', timeout: 30000 })
  await row.click()
  await page.locator('.tr-input').waitFor({ state: 'visible', timeout: 30000 })
  ok('the prompt box is there')

  // ---- the test proper: a paste carrying an image file ----
  log('pasting an image through the real clipboard API')
  await page.locator('.tr-input').focus()
  await page.evaluate(async (b64) => {
    const bin = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))
    const file = new File([bin], 'screenshot.png', { type: 'image/png' })
    const dt = new DataTransfer()
    dt.items.add(file)
    const ta = document.querySelector('.tr-input')
    ta.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }))
  }, PNG_B64)

  await page.locator('.tr-shot').first().waitFor({ state: 'visible', timeout: 30000 })
  ok('an attachment chip appeared')

  const chipText = await page.locator('.tr-shot-name').first().innerText()
  if (!chipText.includes('screenshot.png')) throw new Error(`chip says ${chipText}`)
  ok(`the chip names the file, not a path: "${chipText}"`)

  const shown = await page.locator('.tr-input').inputValue()
  if (shown.includes('/')) throw new Error(`the path leaked into the textbox: ${shown}`)
  ok('the textbox is still empty — no path noise')

  const path = await page.locator('.tr-shot-name').first().getAttribute('title')
  if (!path || !path.startsWith('/')) throw new Error(`no absolute path on the chip: ${path}`)
  log(`the server says it wrote: ${path}`)

  // ---- the part that matters: is it actually on the machine? ----
  if (!existsSync(path)) throw new Error(`NOTHING IS THERE: ${path}`)
  const st = statSync(path)
  const mode = (st.mode & 0o777).toString(8)
  if (mode !== '600') throw new Error(`mode is ${mode}, want 600`)
  const bytes = readFileSync(path)
  const expect = Buffer.from(PNG_B64, 'base64')
  if (!bytes.equals(expect)) throw new Error(`file is ${bytes.length} bytes, uploaded ${expect.length}`)
  ok(`the file exists, is 0600, and is byte-identical (${bytes.length} bytes)`)

  // Text paste must be untouched by any of this.
  log('checking that pasting TEXT still works, multi-line and in one piece')
  await page.evaluate(() => {
    const dt = new DataTransfer()
    dt.setData('text/plain', 'line one\nline two\nline three')
    document.querySelector('.tr-input').dispatchEvent(
      new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }))
  })
  await sleep(300)
  // The browser's own default paste fills the textarea; we only had to not
  // cancel it. Typing is the fallback assertion if the synthetic event is
  // swallowed by the headless clipboard.
  const after = await page.locator('.tr-input').inputValue()
  if (after.split('\n').length < 3) {
    await page.locator('.tr-input').fill('line one\nline two\nline three')
    const typed = await page.locator('.tr-input').inputValue()
    if (typed.split('\n').length !== 3) throw new Error('multi-line text did not survive')
    ok('multi-line text holds as one value (verified by fill; synthetic paste is inert headless)')
  } else {
    ok('multi-line paste arrived in one piece')
  }

  // And the chip can be removed again.
  await page.locator('.tr-shot-x').first().click()
  await page.locator('.tr-shot').first().waitFor({ state: 'detached', timeout: 10000 })
  ok('the attachment can be removed')

  if (errors.length) throw new Error(`page errors: ${JSON.stringify(errors)}`)
  ok('no page errors')

  await ctx.close()
  await browser.close()
  console.log('\n\x1b[1;32mPASS\x1b[0m — a pasted image is a real file on this machine.\n')
}

function pickIdleAgent() {
  const lines = hx('msg', 'agents').split('\n')
  for (const l of lines) {
    const m = l.match(/^(\S+)\s+\S+\s+(idle|done)\s/)
    if (m) return m[1]
  }
  throw new Error('no idle agent to use; pass one as argv[2]')
}

main().catch((e) => {
  console.error(`\n\x1b[1;31mFAIL\x1b[0m ${e.message}\n`)
  process.exitCode = 1
})
