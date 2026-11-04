// SDK openai (Node) contra o modo public, os gratuitos e o Extra Usage do
// opencode (task 425; ADR 0072, itens 4–7 e 12; spec 006 16.3). A mesma
// matriz de test_opencode_free.py. O User-Agent do OpenCode (opencode/...)
// só vai ao wwz local: a suíte se recusa a rodar contra o opencode.ai.

import assert from 'node:assert/strict';
import { randomInt } from 'node:crypto';
import { before, describe, test } from 'node:test';
import OpenAI from 'openai';
import { BASE_URL, MESSAGES, SKIP } from './helpers.mjs';

const PAYG = '/opencode/zen/v1';
const GO = '/opencode/zen/go/v1';
const OC_UA = 'opencode/latest/2.2.5/wwz-sdk';
const FREE = 'wwz-free';
const PAID = 'wwz-fast';
const EXTRA_KEY = 'sk-wwz-oc-extra';
const FREE_TEXT =
  'Free usage limit reached. Free models work in OpenCode (https://opencode.ai); ' +
  'subscribe to OpenCode Go to keep going: https://opencode.ai/go';

// Cada cliente tem um IP próprio (X-Forwarded-For; trusted_proxies de
// loopback em tests/sdk/wwz.yaml): a cota diária dos gratuitos é por IP sem
// chave, e a suíte pode rodar de novo no mesmo servidor.
function client(base, { key = 'public', ua } = {}) {
  const headers = {
    'x-opencode-session': 'ses_wwz_sdk',
    'X-Forwarded-For': `2001:db8::${randomInt(65536).toString(16)}:${randomInt(65536).toString(16)}`,
  };
  if (ua) headers['User-Agent'] = ua;
  return new OpenAI({ baseURL: BASE_URL + base, apiKey: key, maxRetries: 0, timeout: 30_000, defaultHeaders: headers });
}

async function caught(promise) {
  try {
    await promise;
  } catch (err) {
    return err;
  }
  return assert.fail('a chamada não falhou');
}

function secondsToMidnight() {
  const now = new Date();
  const midnight = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() + 1);
  return (midnight - now.getTime()) / 1000;
}

function assertFreeUsageLimit(err) {
  assert.ok(err instanceof OpenAI.RateLimitError, `${err?.constructor?.name}: ${err}`);
  assert.equal(err.status, 429);
  assert.deepEqual(err.error, { type: 'FreeUsageLimitError', message: FREE_TEXT });
  const ra = Number(err.headers.get('retry-after'));
  assert.ok(Math.abs(ra - secondsToMidnight()) <= 3, `retry-after ${ra}`);
}

const chat = (c, model) => c.chat.completions.create({ model, messages: MESSAGES });

describe('opencode: public, gratuitos e Extra Usage', { skip: SKIP }, () => {
  before(() => {
    // O header do OpenCode contorna um controle de acesso do real: só ao wwz.
    assert.ok(!BASE_URL.includes('opencode.ai'), BASE_URL);
  });

  test('public com o cliente OpenCode atende o gratuito como free', async () => {
    const r = await chat(client(PAYG, { ua: OC_UA }), FREE);
    assert.ok(r.choices[0].message.content);
    assert.equal(r.usage.opencode.billing_source, 'free');
    assert.equal(r.usage.cost, 0);
  });

  test('public com cliente genérico: 429 FreeUsageLimitError (cota zero)', async () => {
    assertFreeUsageLimit(await caught(chat(client(PAYG), FREE)));
  });

  test('public com modelo pago: AuthenticationError AuthError', async () => {
    const err = await caught(chat(client(PAYG, { ua: OC_UA }), PAID));
    assert.ok(err instanceof OpenAI.AuthenticationError, `${err?.constructor?.name}: ${err}`);
    assert.deepEqual(err.error, { type: 'AuthError', message: 'Missing API key.' });
  });

  test('public na base Go: AuthenticationError Go requires a credential', async () => {
    const err = await caught(chat(client(GO, { ua: OC_UA }), FREE));
    assert.ok(err instanceof OpenAI.AuthenticationError, `${err?.constructor?.name}: ${err}`);
    assert.deepEqual(err.error, { type: 'server_error', message: 'Upstream request failed: Go requires a credential' });
  });

  test('free_requests_per_day: 2 -> a 3ª dá RateLimitError', async () => {
    const c = client(PAYG, { ua: OC_UA });
    for (let i = 0; i < 2; i++) {
      assert.equal((await chat(c, FREE)).usage.opencode.billing_source, 'free');
    }
    assertFreeUsageLimit(await caught(chat(c, FREE)));
  });

  test('cota Go esgotada com Extra Usage: billing_source balance', async () => {
    const c = client(GO, { key: EXTRA_KEY });
    await chat(c, PAID);
    const r = await chat(c, PAID);
    assert.equal(r.usage.opencode.billing_source, 'balance');
    assert.ok(r.usage.cost > 0, `cost ${r.usage.cost}`);
  });
});
