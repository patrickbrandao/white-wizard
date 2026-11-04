// SDK @anthropic-ai/sdk (Node) contra o driver anthropic (spec 018).

import assert from 'node:assert/strict';
import { describe, test } from 'node:test';
import Anthropic from '@anthropic-ai/sdk';
import {
  BAD_KEY,
  MESSAGES,
  MODELS,
  RL_KEY,
  SKIP,
  anthropicClient,
  firstError,
  retryAfterSeconds,
} from './helpers.mjs';

const MAX_TOKENS = 2048;

const WEATHER_TOOL = {
  name: 'get_weather',
  description: 'Weather for a city.',
  input_schema: {
    type: 'object',
    properties: {
      city: { type: 'string' },
      days: { type: 'integer' },
    },
    required: ['city'],
  },
};

const THINKING = { type: 'enabled', budget_tokens: 1024 };

function textOf(message) {
  return message.content
    .filter((b) => b.type === 'text')
    .map((b) => b.text)
    .join('');
}

function checkUsage(usage) {
  assert.ok(usage.input_tokens > 0);
  assert.ok(usage.output_tokens > 0);
}

function checkToolUse(block) {
  assert.equal(block.type, 'tool_use');
  assert.ok(block.id.startsWith('toolu_'), block.id);
  assert.equal(block.name, 'get_weather');
  assert.equal(typeof block.input, 'object');
  assert.equal(typeof block.input.city, 'string');
  if ('days' in block.input) assert.ok(Number.isInteger(block.input.days));
}

describe('anthropic sdk / driver anthropic', { skip: SKIP }, () => {
  const anth = SKIP ? null : anthropicClient();

  test('chat sem stream', async () => {
    const m = await anth.messages.create({
      model: 'wwz-fast',
      max_tokens: MAX_TOKENS,
      messages: MESSAGES,
    });
    assert.equal(m.type, 'message');
    assert.equal(m.role, 'assistant');
    assert.ok(m.id.startsWith('msg_'), m.id);
    assert.equal(m.stop_reason, 'end_turn');
    assert.ok(textOf(m));
    checkUsage(m.usage);
  });

  test('chat com stream (messages.stream)', async () => {
    const params = { model: 'wwz-fast', max_tokens: MAX_TOKENS, messages: MESSAGES };
    const ref = await anth.messages.create(params);
    const s = anth.messages.stream(params);
    let text = '';
    s.on('text', (delta) => {
      text += delta;
    });
    const final = await s.finalMessage();
    assert.equal(text, textOf(final));
    // Mesma requisição com seed fixa: o texto montado é o content.
    assert.equal(text, textOf(ref));
    assert.equal(final.stop_reason, 'end_turn');
    checkUsage(final.usage);
    assert.equal(final.usage.output_tokens, ref.usage.output_tokens);
  });

  test('chat com stream: true (eventos brutos)', async () => {
    const stream = await anth.messages.create({
      model: 'wwz-fast',
      max_tokens: MAX_TOKENS,
      messages: MESSAGES,
      stream: true,
    });
    const types = [];
    for await (const ev of stream) types.push(ev.type);
    assert.equal(types[0], 'message_start');
    assert.equal(types.at(-1), 'message_stop');
    assert.ok(types.includes('content_block_start'));
    assert.ok(types.includes('content_block_delta'));
    assert.ok(types.includes('content_block_stop'));
    assert.ok(types.includes('message_delta'));
  });

  test('tool call sem stream', async () => {
    const m = await anth.messages.create({
      model: 'wwz-tools',
      max_tokens: MAX_TOKENS,
      messages: MESSAGES,
      tools: [WEATHER_TOOL],
    });
    assert.equal(m.stop_reason, 'tool_use');
    const uses = m.content.filter((b) => b.type === 'tool_use');
    assert.ok(uses.length);
    for (const b of uses) checkToolUse(b);
  });

  test('tool call com stream', async () => {
    const s = anth.messages.stream({
      model: 'wwz-tools',
      max_tokens: MAX_TOKENS,
      messages: MESSAGES,
      tools: [WEATHER_TOOL],
    });
    let partial = 0;
    s.on('inputJson', () => {
      partial++;
    });
    const final = await s.finalMessage();
    assert.equal(final.stop_reason, 'tool_use');
    const uses = final.content.filter((b) => b.type === 'tool_use');
    assert.ok(uses.length);
    assert.ok(partial > 0, 'stream sem input_json_delta');
    for (const b of uses) checkToolUse(b);
  });

  test('raciocínio sem stream', async () => {
    const m = await anth.messages.create({
      model: 'wwz-reason',
      max_tokens: MAX_TOKENS,
      messages: MESSAGES,
      thinking: THINKING,
    });
    const kinds = m.content.map((b) => b.type);
    assert.equal(kinds[0], 'thinking', String(kinds));
    assert.ok(kinds.includes('text'));
    assert.ok(m.content[0].thinking);
    assert.ok(m.content[0].signature);
  });

  test('raciocínio com stream', async () => {
    const s = anth.messages.stream({
      model: 'wwz-reason',
      max_tokens: MAX_TOKENS,
      messages: MESSAGES,
      thinking: THINKING,
    });
    const events = [];
    for (const name of ['thinking', 'signature', 'text']) s.on(name, () => events.push(name));
    const final = await s.finalMessage();
    assert.ok(events.includes('thinking'));
    assert.ok(events.includes('signature'));
    assert.ok(events.indexOf('thinking') < events.indexOf('text'));
    assert.equal(final.content[0].type, 'thinking');
    assert.ok(final.content[0].thinking);
    assert.ok(final.content[0].signature);
    assert.ok(textOf(final));
  });

  test('401 com chave inválida', async () => {
    const bad = anthropicClient(BAD_KEY);
    await assert.rejects(
      bad.messages.create({ model: 'wwz-fast', max_tokens: MAX_TOKENS, messages: MESSAGES }),
      (err) => {
        assert.ok(err instanceof Anthropic.AuthenticationError, `${err?.constructor?.name}: ${err}`);
        assert.equal(err.status, 401);
        assert.equal(err.error?.type, 'error');
        assert.equal(err.error?.error?.type, 'authentication_error');
        return true;
      },
    );
  });

  test('429 com retry-after', async () => {
    const rl = anthropicClient(RL_KEY);
    // rpm: 1 compartilhado com os outros drivers: a segunda chamada
    // sempre bate no limite.
    const err = await firstError(2, () =>
      rl.messages.create({ model: 'wwz-rl', max_tokens: MAX_TOKENS, messages: MESSAGES }),
    );
    assert.ok(err instanceof Anthropic.RateLimitError, `${err?.constructor?.name}: ${err}`);
    assert.equal(err.status, 429);
    assert.equal(err.error?.error?.type, 'rate_limit_error');
    retryAfterSeconds(err.headers);
  });

  test('GET /v1/models e retrieve', async () => {
    const ids = [];
    for await (const m of anth.models.list()) ids.push(m.id);
    for (const want of MODELS) assert.ok(ids.includes(want), `${want} ausente em ${ids}`);
    const got = await anth.models.retrieve('wwz-fast');
    assert.equal(got.id, 'wwz-fast');
    assert.equal(got.type, 'model');
  });
});
