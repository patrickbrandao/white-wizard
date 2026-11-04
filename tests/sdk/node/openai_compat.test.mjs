// SDK openai (Node) contra os drivers openai, openrouter e opencode (spec 018).

import assert from 'node:assert/strict';
import { describe, test } from 'node:test';
import OpenAI from 'openai';
import {
  BAD_KEY,
  MESSAGES,
  MODELS,
  OPENAI_COMPAT,
  RL_KEY,
  SKIP,
  firstError,
  openaiClient,
  retryAfterSeconds,
} from './helpers.mjs';

const WEATHER_TOOL = {
  type: 'function',
  function: {
    name: 'get_weather',
    description: 'Weather for a city.',
    parameters: {
      type: 'object',
      properties: {
        city: { type: 'string' },
        days: { type: 'integer' },
      },
      required: ['city'],
    },
  },
};

// model da resposta: o slug no openrouter (ADR 0038), o id nos demais.
function responseModel(driver, id) {
  if (driver === 'openrouter' && !id.includes('/')) return `white-wizard/${id}`;
  return id;
}

function checkUsage(usage) {
  assert.ok(usage, 'usage ausente');
  assert.ok(usage.prompt_tokens > 0);
  assert.ok(usage.completion_tokens > 0);
  assert.equal(usage.total_tokens, usage.prompt_tokens + usage.completion_tokens);
}

function checkToolCall(call) {
  assert.equal(call.type, 'function');
  assert.ok(call.id);
  assert.equal(call.function.name, 'get_weather');
  const args = JSON.parse(call.function.arguments);
  assert.equal(typeof args, 'object');
  assert.equal(typeof args.city, 'string');
  if ('days' in args) assert.ok(Number.isInteger(args.days), `days não inteiro: ${args.days}`);
}

// O SDK Node não descarta campos fora do tipo: os dialetos de raciocínio
// (ADR 0022) ficam no próprio objeto.
function reasoningParams(driver) {
  if (driver === 'openrouter') return { reasoning: { effort: 'low' } };
  return { reasoning_effort: 'low' };
}

for (const driver of Object.keys(OPENAI_COMPAT)) {
  describe(`openai sdk / driver ${driver}`, { skip: SKIP }, () => {
    const oai = SKIP ? null : openaiClient(driver);

    test('chat sem stream', async () => {
      const r = await oai.chat.completions.create({ model: 'wwz-fast', messages: MESSAGES });
      assert.equal(r.object, 'chat.completion');
      assert.equal(r.model, responseModel(driver, 'wwz-fast'));
      assert.ok(r.id);
      const choice = r.choices[0];
      assert.equal(choice.message.role, 'assistant');
      assert.ok(choice.message.content);
      assert.equal(choice.finish_reason, 'stop');
      checkUsage(r.usage);
    });

    test('chat com stream', async () => {
      const ref = await oai.chat.completions.create({ model: 'wwz-fast', messages: MESSAGES });
      const stream = await oai.chat.completions.create({
        model: 'wwz-fast',
        messages: MESSAGES,
        stream: true,
        stream_options: { include_usage: true },
      });
      let text = '';
      let usage = null;
      let finish = null;
      const ids = new Set();
      for await (const chunk of stream) {
        assert.equal(chunk.object, 'chat.completion.chunk');
        assert.equal(chunk.model, responseModel(driver, 'wwz-fast'));
        ids.add(chunk.id);
        if (chunk.choices?.length) {
          const c = chunk.choices[0];
          if (c.delta?.content) text += c.delta.content;
          if (c.finish_reason) finish = c.finish_reason;
        }
        if (chunk.usage) usage = chunk.usage;
      }
      assert.equal(ids.size, 1, `ids diferentes no mesmo stream: ${[...ids]}`);
      assert.equal(finish, 'stop');
      // Mesma requisição com seed fixa: o texto montado é o content.
      assert.equal(text, ref.choices[0].message.content);
      checkUsage(usage);
      assert.equal(usage.completion_tokens, ref.usage.completion_tokens);
    });

    test('helper chat.completions.stream', async () => {
      const s = oai.chat.completions.stream({ model: 'wwz-fast', messages: MESSAGES });
      let deltas = '';
      s.on('content.delta', (e) => {
        deltas += e.delta;
      });
      const final = await s.finalChatCompletion();
      assert.ok(deltas);
      assert.equal(final.choices[0].message.content, deltas);
    });

    test('tool call sem stream', async () => {
      const r = await oai.chat.completions.create({
        model: 'wwz-tools',
        messages: MESSAGES,
        tools: [WEATHER_TOOL],
      });
      const choice = r.choices[0];
      assert.equal(choice.finish_reason, 'tool_calls');
      assert.ok(choice.message.tool_calls?.length);
      for (const call of choice.message.tool_calls) checkToolCall(call);
      checkUsage(r.usage);
    });

    test('tool call com stream', async () => {
      const s = oai.chat.completions.stream({
        model: 'wwz-tools',
        messages: MESSAGES,
        tools: [WEATHER_TOOL],
      });
      const done = [];
      s.on('tool_calls.function.arguments.done', (e) => done.push(e));
      const final = await s.finalChatCompletion();
      assert.ok(done.length, 'stream sem tool_calls.function.arguments.done');
      const choice = final.choices[0];
      assert.equal(choice.finish_reason, 'tool_calls');
      assert.ok(choice.message.tool_calls?.length);
      for (const call of choice.message.tool_calls) checkToolCall(call);
      // Acumulado pelo SDK = argumentos completos de cada evento done.
      assert.deepEqual(
        done.map((e) => JSON.parse(e.arguments)),
        choice.message.tool_calls.map((c) => JSON.parse(c.function.arguments)),
      );
    });

    test('raciocínio sem stream', async () => {
      const r = await oai.chat.completions.create({
        model: 'wwz-reason',
        messages: MESSAGES,
        ...reasoningParams(driver),
      });
      const msg = r.choices[0].message;
      assert.ok(msg.content);
      assert.ok(r.usage.completion_tokens_details?.reasoning_tokens > 0);
      if (driver === 'openai') {
        // ADR 0022: Chat Completions real esconde o texto do raciocínio.
        assert.ok(!('reasoning_content' in msg));
        assert.ok(!('reasoning' in msg));
      } else if (driver === 'opencode') {
        assert.ok(msg.reasoning_content);
        assert.ok(!('reasoning' in msg));
      } else {
        assert.ok(msg.reasoning);
        assert.ok(!('reasoning_content' in msg));
      }
    });

    test('raciocínio com stream', async () => {
      const stream = await oai.chat.completions.create({
        model: 'wwz-reason',
        messages: MESSAGES,
        stream: true,
        stream_options: { include_usage: true },
        ...reasoningParams(driver),
      });
      const order = [];
      let reasoning = '';
      let usage = null;
      for await (const chunk of stream) {
        if (chunk.usage) usage = chunk.usage;
        if (!chunk.choices?.length) continue;
        const delta = chunk.choices[0].delta ?? {};
        for (const name of ['reasoning_content', 'reasoning']) {
          if (delta[name]) {
            reasoning += delta[name];
            order.push(name);
          }
        }
        if (delta.content) order.push('content');
      }
      assert.ok(usage, 'usage ausente');
      assert.ok(usage.completion_tokens_details?.reasoning_tokens > 0);
      assert.ok(order.includes('content'));
      if (driver === 'openai') {
        assert.deepEqual([...new Set(order)], ['content']);
      } else {
        const want = driver === 'opencode' ? 'reasoning_content' : 'reasoning';
        assert.ok(reasoning);
        assert.deepEqual(new Set(order), new Set([want, 'content']));
        // Raciocínio vem antes do primeiro conteúdo visível.
        const first = order.indexOf('content');
        assert.ok(first > order.indexOf(want));
        assert.ok(!order.slice(first).includes(want));
      }
    });

    test('401 com chave inválida', async () => {
      const bad = openaiClient(driver, BAD_KEY);
      await assert.rejects(
        bad.chat.completions.create({ model: 'wwz-fast', messages: MESSAGES }),
        (err) => {
          assert.ok(err instanceof OpenAI.AuthenticationError, `${err?.constructor?.name}: ${err}`);
          assert.equal(err.status, 401);
          return true;
        },
      );
    });

    test('429 com retry-after', async () => {
      const rl = openaiClient(driver, RL_KEY);
      // rpm: 1 compartilhado entre os drivers: a primeira chamada pode
      // passar ou já bater no limite; a segunda sempre bate.
      const err = await firstError(2, () =>
        rl.chat.completions.create({ model: 'wwz-rl', messages: MESSAGES }),
      );
      assert.ok(err instanceof OpenAI.RateLimitError, `${err?.constructor?.name}: ${err}`);
      assert.equal(err.status, 429);
      retryAfterSeconds(err.headers);
    });

    test('GET /v1/models', async () => {
      const ids = [];
      for await (const m of oai.models.list()) ids.push(m.id);
      for (const want of MODELS) {
        assert.ok(
          ids.some((i) => i === want || i.endsWith(`/${want}`)),
          `${want} ausente em ${ids}`,
        );
      }
    });

    test('id listado é aceito e devolvido (ADR 0038)', async () => {
      let listed = null;
      for await (const m of oai.models.list()) {
        if (m.id.split('/').pop() === 'wwz-fast') listed = m.id;
      }
      assert.ok(listed, 'wwz-fast ausente na listagem');
      const r = await oai.chat.completions.create({ model: listed, messages: MESSAGES });
      assert.equal(r.model, listed);
      const stream = await oai.chat.completions.create({ model: listed, messages: MESSAGES, stream: true });
      for await (const chunk of stream) assert.equal(chunk.model, listed);
    });
  });
}

describe('openai sdk / openrouter reasoning_details', { skip: SKIP }, () => {
  test('reasoning_details[] sem e com stream (spec 006)', async () => {
    const client = openaiClient('openrouter');
    const r = await client.chat.completions.create({
      model: 'wwz-reason',
      messages: MESSAGES,
      ...reasoningParams('openrouter'),
    });
    const stream = await client.chat.completions.create({
      model: 'wwz-reason',
      messages: MESSAGES,
      stream: true,
      ...reasoningParams('openrouter'),
    });
    const streamed = [];
    for await (const chunk of stream) {
      if (chunk.choices?.length) streamed.push(...(chunk.choices[0].delta?.reasoning_details ?? []));
    }
    const message = r.choices[0].message;
    const details = message.reasoning_details;
    assert.ok(Array.isArray(details) && details.length, 'reasoning_details[] ausente (spec 006)');
    assert.ok(streamed.length, 'delta.reasoning_details ausente no stream (spec 006)');
    assert.equal(details.map((d) => d.text ?? '').join(''), message.reasoning);
    for (const d of [...details, ...streamed]) {
      assert.ok(String(d.type ?? '').startsWith('reasoning.'), `type inesperado: ${d.type}`);
    }
  });
});

describe('openai sdk / openrouter model', { skip: SKIP }, () => {
  // Resolução do model como no real (task 163).
  test('slug, nome sem autor e caixa resolvem; desconhecido é 400', async () => {
    const client = openaiClient('openrouter');
    for (const asked of ['white-wizard/wwz-fast', 'wwz-fast', 'WWZ-Fast']) {
      const r = await client.chat.completions.create({ model: asked, messages: MESSAGES });
      assert.equal(r.model, 'white-wizard/wwz-fast', asked);
    }
    for (const asked of ['white-wizard/nope', 'nope', 'x/wwz-fast']) {
      const err = await firstError(1, () => client.chat.completions.create({ model: asked, messages: MESSAGES }));
      assert.ok(err instanceof OpenAI.BadRequestError, `${asked}: ${err}`);
      assert.equal(err.status, 400);
      assert.equal(err.error?.message, `${asked} is not a valid model ID`);
    }
  });
});
