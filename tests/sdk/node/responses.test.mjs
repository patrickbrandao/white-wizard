// SDK openai (Node), Responses API, contra openai, openrouter e opencode
// (spec 004 "Responses API", ADRs 0054 e 0055; task 239).
//
// O openai guarda a resposta e aceita previous_response_id (o motor resolve
// a cadeia); os gateways não guardam nada e recusam previous_response_id e
// store: true com 400, como os reais. Neles a conversa encadeada manda os
// itens anteriores no input. O azure (API v1) está em azure.test.mjs.

import assert from 'node:assert/strict';
import { describe, test } from 'node:test';
import OpenAI from 'openai';
import { BAD_KEY, OPENAI_COMPAT, SKIP, openaiClient } from './helpers.mjs';

const PROMPT = 'Say something short.';

const WEATHER_TOOL = {
  type: 'function',
  name: 'get_weather',
  description: 'Weather for a city.',
  parameters: {
    type: 'object',
    properties: { city: { type: 'string' } },
    required: ['city'],
  },
};

const STATEFUL = new Set(['openai']);

// model da resposta: o slug no openrouter (ADR 0038), o id nos demais.
function modelName(driver, id) {
  return driver === 'openrouter' ? `white-wizard/${id}` : id;
}

function checkUsage(usage) {
  assert.ok(usage, 'usage ausente');
  assert.ok(usage.input_tokens > 0);
  assert.ok(usage.output_tokens > 0);
  assert.equal(usage.total_tokens, usage.input_tokens + usage.output_tokens);
  assert.ok(usage.input_tokens_details.cached_tokens >= 0);
  assert.ok(usage.output_tokens_details.reasoning_tokens >= 0);
}

const functionCalls = (r) => r.output.filter((o) => o.type === 'function_call');

function checkCall(call) {
  assert.ok(call.call_id);
  assert.equal(call.name, 'get_weather');
  assert.equal(typeof JSON.parse(call.arguments).city, 'string');
}

async function rejects400(promise, needle) {
  await assert.rejects(promise, (err) => {
    assert.ok(err instanceof OpenAI.BadRequestError, `esperado BadRequestError, veio ${err}`);
    assert.equal(err.status, 400);
    if (needle) assert.match(String(err.message), new RegExp(needle));
    return true;
  });
}

for (const driver of Object.keys(OPENAI_COMPAT)) {
  describe(`openai sdk responses / driver ${driver}`, { skip: SKIP }, () => {
    const oai = SKIP ? null : openaiClient(driver);
    const stateful = STATEFUL.has(driver);

    test('create sem stream', async () => {
      const r = await oai.responses.create({ model: 'wwz-fast', input: PROMPT, instructions: 'Be brief.' });
      assert.equal(r.object, 'response');
      assert.ok(r.id.startsWith(driver === 'openrouter' ? 'gen-' : 'resp_'), r.id);
      assert.equal(r.status, 'completed');
      assert.equal(r.model, modelName(driver, 'wwz-fast'));
      assert.ok(r.output_text);
      const msgs = r.output.filter((o) => o.type === 'message');
      assert.equal(msgs.length, 1);
      assert.equal(msgs[0].role, 'assistant');
      assert.equal(msgs[0].content[0].type, 'output_text');
      checkUsage(r.usage);
    });

    test('create com stream', async () => {
      const ref = await oai.responses.create({ model: 'wwz-fast', input: PROMPT });
      const stream = await oai.responses.create({ model: 'wwz-fast', input: PROMPT, stream: true });
      const types = [];
      const seq = [];
      let text = '';
      let final;
      for await (const ev of stream) {
        types.push(ev.type);
        seq.push(ev.sequence_number);
        if (ev.type === 'response.output_text.delta') text += ev.delta;
        if (ev.type === 'response.completed') final = ev.response;
      }
      assert.equal(types[0], 'response.created');
      assert.equal(types.at(-1), 'response.completed');
      assert.deepEqual(seq, [...seq.keys()]);
      assert.ok(final);
      // Mesma requisição com seed fixa: o texto montado é o da resposta.
      assert.equal(text, ref.output_text);
      checkUsage(final.usage);
      assert.equal(final.usage.output_tokens, ref.usage.output_tokens);
    });

    test('helper de stream', async () => {
      const s = oai.responses.stream({ model: 'wwz-fast', input: PROMPT });
      let deltas = '';
      for await (const e of s) if (e.type === 'response.output_text.delta') deltas += e.delta;
      const final = await s.finalResponse();
      assert.ok(deltas);
      assert.equal(final.output_text, deltas);
    });

    test('incomplete por max_output_tokens', async () => {
      const r = await oai.responses.create({ model: 'wwz-fast', input: PROMPT, max_output_tokens: 16 });
      assert.equal(r.status, 'incomplete');
      assert.equal(r.incomplete_details.reason, 'max_output_tokens');
      assert.equal(r.usage.output_tokens, 16);
    });

    test('tool sem stream', async () => {
      const r = await oai.responses.create({ model: 'wwz-tools', input: PROMPT, tools: [WEATHER_TOOL] });
      const calls = functionCalls(r);
      assert.ok(calls.length > 0);
      calls.forEach(checkCall);
      assert.equal(r.tools[0].name, 'get_weather');
    });

    test('tool com stream', async () => {
      const s = oai.responses.stream({ model: 'wwz-tools', input: PROMPT, tools: [WEATHER_TOOL] });
      const done = [];
      for await (const e of s) if (e.type === 'response.function_call_arguments.done') done.push(e);
      const final = await s.finalResponse();
      const calls = functionCalls(final);
      assert.ok(done.length > 0 && calls.length > 0);
      calls.forEach(checkCall);
      assert.deepEqual(
        done.map((e) => JSON.parse(e.arguments)),
        calls.map((c) => JSON.parse(c.arguments)),
      );
    });

    test('encadeado com os itens no input', async () => {
      const r1 = await oai.responses.create({ model: 'wwz-tools', input: PROMPT, tools: [WEATHER_TOOL], store: false });
      const call = functionCalls(r1)[0];
      const items = [{ role: 'user', content: PROMPT }, ...r1.output];
      items.push({ type: 'function_call_output', call_id: call.call_id, output: '22C, sunny' });
      const r2 = await oai.responses.create({ model: 'wwz-fast', input: items, store: false });
      assert.equal(r2.status, 'completed');
      assert.ok(r2.output_text);
      const alone = await oai.responses.create({ model: 'wwz-fast', input: PROMPT, store: false });
      assert.ok(r2.usage.input_tokens > alone.usage.input_tokens);
    });

    test('previous_response_id', async () => {
      const r1 = await oai.responses.create({ model: 'wwz-fast', input: PROMPT, store: stateful });
      if (!stateful) {
        // O gateway não guarda respostas: 400 (ADR 0055).
        await rejects400(
          oai.responses.create({ model: 'wwz-fast', input: 'And now?', previous_response_id: r1.id }),
          'previous_response_id',
        );
        return;
      }
      const r2 = await oai.responses.create({ model: 'wwz-fast', input: 'And now?', previous_response_id: r1.id });
      assert.equal(r2.previous_response_id, r1.id);
      assert.equal(r2.status, 'completed');
      assert.ok(r2.usage.input_tokens > r1.usage.input_tokens);
      const s = oai.responses.stream({ model: 'wwz-fast', input: 'Once more.', previous_response_id: r2.id });
      const final = await s.finalResponse();
      assert.equal(final.previous_response_id, r2.id);
      assert.ok(final.usage.input_tokens > r2.usage.input_tokens);
    });

    test('volta de tool pela cadeia', { skip: stateful ? false : 'gateway sem estado' }, async () => {
      const r1 = await oai.responses.create({ model: 'wwz-tools', input: PROMPT, tools: [WEATHER_TOOL] });
      const call = functionCalls(r1)[0];
      const r2 = await oai.responses.create({
        model: 'wwz-fast',
        previous_response_id: r1.id,
        input: [{ type: 'function_call_output', call_id: call.call_id, output: '22C' }],
      });
      assert.equal(r2.status, 'completed');
      assert.ok(r2.output_text);
    });

    test('previous_response_id inexistente', async () => {
      const missing = `resp_${'0'.repeat(50)}`;
      await assert.rejects(
        oai.responses.create({ model: 'wwz-fast', input: 'x', previous_response_id: missing }),
        (err) => {
          assert.ok(err instanceof OpenAI.BadRequestError, String(err));
          if (stateful) {
            assert.equal(err.code, 'previous_response_not_found');
            assert.equal(err.param, 'previous_response_id');
          }
          return true;
        },
      );
    });

    test('store: true', async () => {
      if (stateful) {
        const r = await oai.responses.create({ model: 'wwz-fast', input: PROMPT, store: true });
        assert.equal(r.store, true);
        return;
      }
      await rejects400(oai.responses.create({ model: 'wwz-fast', input: PROMPT, store: true }));
    });

    test('401 com chave inválida', async () => {
      const bad = openaiClient(driver, BAD_KEY);
      await assert.rejects(bad.responses.create({ model: 'wwz-fast', input: PROMPT }), (err) => {
        assert.ok(err instanceof OpenAI.AuthenticationError, String(err));
        assert.equal(err.status, 401);
        return true;
      });
    });
  });
}
