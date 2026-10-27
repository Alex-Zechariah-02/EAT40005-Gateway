import { expect, test } from '@playwright/test'
import { resolveConfig } from 'vite'
import { adminURL, build, controls, Demo, gatewayIdentity, siteURL } from './env'
import { Demo as DashboardDemo } from '../../dashboard/e2e/env'

test('the browser harness and development proxy honor the selected isolation ports', async () => {
  // Independent expectations keep an offset accidentally omitted by the helper
  // from choosing both the request target and its expected value.
  const offset = Number(process.env.EAT_PORT_OFFSET ?? 0)
  expect(adminURL).toBe(`http://127.0.0.1:${9440 + offset}`)
  expect(siteURL).toBe(`http://127.0.0.2:${9480 + offset}`)
  expect(Object.values(controls)).toEqual([
    `http://127.0.0.1:${9601 + offset}`,
    `http://127.0.0.1:${9602 + offset}`,
    `http://127.0.0.1:${9603 + offset}`,
    `http://127.0.0.1:${9604 + offset}`,
    `http://127.0.0.1:${9605 + offset}`,
  ])
  const config = await resolveConfig({}, 'serve')
  const proxy = config.server.proxy!['^/((inventory|orders|reports|accounts)(/|$)|support/)']
  expect(proxy).toMatchObject({ target: `http://127.0.0.2:${9480 + offset}` })
  if (process.env.E2E_DEV_PORT) expect(config.server.port).toBe(Number(process.env.E2E_DEV_PORT))
})

test('the selected backend records its actual source identity', () => {
  build()
  expect(gatewayIdentity()).toMatchObject({ source: process.env.E2E_BACKEND_SOURCE ?? 'head' })
  expect(gatewayIdentity()).toHaveProperty('source_sha256', expect.stringMatching(/^[a-f0-9]{64}$/))
  expect(gatewayIdentity()).toHaveProperty('diff_sha256', expect.stringMatching(/^[a-f0-9]{64}$/))
})

test('failed demo startup can still be cleaned up by either browser suite', async () => {
  const bin = build()
  for (const [name, Deployment] of [['client', Demo], ['dashboard', DashboardDemo]] as const) {
    // Deliberately omit setup: the owned demo exits before opening listeners.
    const demo = new Deployment(bin, '')
    await expect(demo.start()).rejects.toThrow('demo run exited')
    const stopped = await Promise.race([
      demo.stop().then(() => true),
      new Promise<boolean>((done) => setTimeout(() => done(false), 1000)),
    ])
    expect.soft(stopped, `${name} teardown waited for an exit that already happened`).toBe(true)
  }
})
