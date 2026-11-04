// SDK @anthropic-ai/sdk contra o modo estrito do cliente (spec 020, spec 018,
// ADR 0074). No /anthropic-cc (client_verification: claude-code) só o Claude
// Code 2.1.308 passa: o SDK não acrescenta ?beta=true, então messages.create e
// beta.messages.create recebem 403 forged_request_error com reason=query (a
// PermissionDeniedError do SDK). No /anthropic (sem a opção) a mesma chamada dá
// 200.

import assert from 'node:assert/strict';
import { describe, test } from 'node:test';
import Anthropic from '@anthropic-ai/sdk';

import { ANTHROPIC_MOUNT, BASE_URL, KEY, SKIP } from './helpers.mjs';

const CC_MOUNT = '/anthropic-cc';
const MESSAGES = [{ role: 'user', content: 'Hello there, reply with ok' }];
const PREFIX =
  'Client verification failed: this request does not match Claude Code 2.1.308; ' +
  'the client is forging Claude Code requests (query: ';

// credential: x-api-key (apiKey) ou Authorization: Bearer (authToken), como
// nas capturas da spec 020.
function client(mount, credential) {
  const auth = credential === 'apiKey' ? { apiKey: KEY } : { apiKey: null, authToken: KEY };
  return new Anthropic({ baseURL: BASE_URL + mount, maxRetries: 0, timeout: 30_000, ...auth });
}

function create(c, beta) {
  if (beta) {
    return c.beta.messages.create({
      model: 'wwz-fast',
      max_tokens: 16,
      messages: MESSAGES,
      betas: ['claude-code-20250219'],
    });
  }
  return c.messages.create({ model: 'wwz-fast', max_tokens: 16, messages: MESSAGES });
}

describe('anthropic: modo estrito do cliente (/anthropic-cc)', { skip: SKIP }, () => {
  for (const beta of [false, true]) {
    for (const credential of ['apiKey', 'authToken']) {
      const name = `${beta ? 'beta.messages' : 'messages'}.create (${credential}) → 403 forged_request_error`;
      test(name, async () => {
        await assert.rejects(create(client(CC_MOUNT, credential), beta), (err) => {
          assert.ok(err instanceof Anthropic.PermissionDeniedError, `${err?.constructor?.name}: ${err}`);
          assert.equal(err.status, 403);
          assert.equal(err.error?.type, 'error');
          assert.equal(err.error?.error?.type, 'forged_request_error');
          assert.ok(err.error?.error?.message?.startsWith(PREFIX), err.error?.error?.message);
          assert.match(err.error?.request_id ?? '', /^req_/);
          assert.equal(err.headers.get('x-wwz-client-verification'), 'failed; reason=query');
          assert.equal(err.headers.get('x-should-retry'), 'false');
          assert.equal(err.headers.get('request-id'), err.error.request_id);
          return true;
        });
      });
    }
  }

  for (const beta of [false, true]) {
    test(`${beta ? 'beta.messages' : 'messages'}.create no /anthropic → 200`, async () => {
      const m = await create(client(ANTHROPIC_MOUNT, 'apiKey'), beta);
      assert.equal(m.type, 'message');
      assert.equal(m.role, 'assistant');
      assert.ok(m.content.some((b) => b.type === 'text' && b.text.length > 0));
    });
  }
});
