// SDK openai (Node) com AzureOpenAI contra o driver azure (spec 018, ADR 0043).
//
// new AzureOpenAI({endpoint: <base>/azure, apiKey, apiVersion}) chama as
// rotas por deployment (/openai/deployments/{d}/...?api-version=) com
// api-key; new OpenAI({baseURL: <base>/azure/openai/v1}) chama a API v1 com
// Bearer (spec 006, 16.6).

import assert from 'node:assert/strict';
import { describe, test } from 'node:test';
import OpenAI, { AzureOpenAI } from 'openai';
import { BAD_KEY, BASE_URL, KEY, MESSAGES, MODELS, RL_KEY, SKIP, firstError } from './helpers.mjs';

const API_VERSION = '2024-10-21';
const TIMEOUT_MS = 30_000;

const WEATHER_TOOL = {
  type: 'function',
  function: {
    name: 'get_weather',
    description: 'Weather for a city.',
    parameters: { type: 'object', properties: { city: { type: 'string' } }, required: ['city'] },
  },
};

function azureClient(key) {
  return new AzureOpenAI({
    endpoint: `${BASE_URL}/azure`,
    apiKey: key || KEY,
    apiVersion: API_VERSION,
    maxRetries: 0,
    timeout: TIMEOUT_MS,
  });
}

function checkUsage(usage) {
  assert.ok(usage, 'usage ausente');
  assert.ok(usage.prompt_tokens > 0);
  assert.ok(usage.completion_tokens > 0);
  assert.equal(usage.total_tokens, usage.prompt_tokens + usage.completion_tokens);
}

function checkPromptFilter(obj) {
  const pfr = obj.prompt_filter_results;
  assert.ok(pfr?.length, `sem prompt_filter_results: ${JSON.stringify(obj)}`);
  assert.equal(pfr[0].prompt_index, 0);
  for (const cat of ['hate', 'self_harm', 'sexual', 'violence']) {
    assert.deepEqual(pfr[0].content_filter_results[cat], { filtered: false, severity: 'safe' });
  }
}

describe('AzureOpenAI / driver azure', { skip: SKIP }, () => {
  const az = SKIP ? null : azureClient();

  test('chat sem stream', async () => {
    const r = await az.chat.completions.create({ model: 'wwz-fast', messages: MESSAGES });
    assert.equal(r.object, 'chat.completion');
    // O model da resposta é o id do modelo, não o nome do deployment.
    assert.equal(r.model, 'wwz-fast');
    assert.match(r.id, /^chatcmpl-/);
    assert.ok(r.choices[0].message.content);
    assert.equal(r.choices[0].finish_reason, 'stop');
    checkUsage(r.usage);
    checkPromptFilter(r);
    assert.equal(r.choices[0].content_filter_results.violence.severity, 'safe');
  });

  test('chat com stream', async () => {
    const ref = await az.chat.completions.create({ model: 'wwz-fast', messages: MESSAGES });
    const stream = await az.chat.completions.create({
      model: 'wwz-fast',
      messages: MESSAGES,
      stream: true,
      stream_options: { include_usage: true },
    });
    const chunks = [];
    for await (const chunk of stream) chunks.push(chunk);
    // Primeiro evento: só prompt_filter_results, sem choices (spec 006, 16.6).
    assert.deepEqual(chunks[0].choices, []);
    assert.equal(chunks[0].id, '');
    checkPromptFilter(chunks[0]);
    let text = '';
    let usage = null;
    let finish = null;
    for (const chunk of chunks.slice(1)) {
      assert.equal(chunk.model, 'wwz-fast');
      if (chunk.choices?.length) {
        if (chunk.choices[0].delta?.content) text += chunk.choices[0].delta.content;
        if (chunk.choices[0].finish_reason) finish = chunk.choices[0].finish_reason;
      }
      if (chunk.usage) usage = chunk.usage;
    }
    assert.equal(finish, 'stop');
    assert.equal(text, ref.choices[0].message.content);
    checkUsage(usage);
  });

  test('helper chat.completions.stream', async () => {
    const s = az.chat.completions.stream({ model: 'wwz-fast', messages: MESSAGES });
    let deltas = '';
    s.on('content.delta', (e) => {
      deltas += e.delta;
    });
    const final = await s.finalChatCompletion();
    assert.ok(deltas);
    assert.equal(final.choices[0].message.content, deltas);
  });

  test('tool call', async () => {
    const r = await az.chat.completions.create({ model: 'wwz-tools', messages: MESSAGES, tools: [WEATHER_TOOL] });
    assert.equal(r.choices[0].finish_reason, 'tool_calls');
    for (const call of r.choices[0].message.tool_calls) {
      assert.equal(call.function.name, 'get_weather');
      assert.equal(typeof JSON.parse(call.function.arguments).city, 'string');
    }
    const s = az.chat.completions.stream({ model: 'wwz-tools', messages: MESSAGES, tools: [WEATHER_TOOL] });
    const done = [];
    s.on('tool_calls.function.arguments.done', (e) => done.push(e));
    const final = await s.finalChatCompletion();
    assert.ok(done.length);
    assert.equal(final.choices[0].finish_reason, 'tool_calls');
  });

  test('raciocínio oculto', async () => {
    const r = await az.chat.completions.create({ model: 'wwz-reason', messages: MESSAGES, reasoning_effort: 'low' });
    assert.ok(r.choices[0].message.content);
    assert.ok(r.usage.completion_tokens_details?.reasoning_tokens > 0);
    assert.ok(!('reasoning_content' in r.choices[0].message));
  });

  test('completions legado', async () => {
    const r = await az.completions.create({ model: 'wwz-fast', prompt: 'Say something', max_tokens: 8 });
    assert.equal(r.object, 'text_completion');
    assert.match(r.id, /^cmpl-/);
    assert.ok(r.choices[0].text);
  });

  test('401 com chave inválida', async () => {
    const bad = azureClient(BAD_KEY);
    const err = await firstError(1, () => bad.chat.completions.create({ model: 'wwz-fast', messages: MESSAGES }));
    assert.ok(err instanceof OpenAI.AuthenticationError, String(err));
    assert.equal(err.status, 401);
    assert.equal(err.code, '401');
  });

  test('404 DeploymentNotFound', async () => {
    const err = await firstError(1, () => az.chat.completions.create({ model: 'no-such-deployment', messages: MESSAGES }));
    assert.ok(err instanceof OpenAI.NotFoundError, String(err));
    assert.equal(err.code, 'DeploymentNotFound');
  });

  test('400 argumento desconhecido', async () => {
    const err = await firstError(1, () => az.chat.completions.create({ model: 'wwz-fast', messages: MESSAGES, bogus: 1 }));
    assert.ok(err instanceof OpenAI.BadRequestError, String(err));
    assert.match(err.message, /Unrecognized request argument supplied: bogus/);
  });

  test('429 com retry-after-ms', async () => {
    const rl = azureClient(RL_KEY);
    const err = await firstError(2, () => rl.chat.completions.create({ model: 'wwz-rl', messages: MESSAGES }));
    assert.ok(err instanceof OpenAI.RateLimitError, String(err));
    assert.equal(err.status, 429);
    assert.equal(err.code, '429');
    // Deployments Standard mandam retry-after-ms (spec 006, 16.6).
    assert.ok(Number(err.headers.get('retry-after-ms')) > 0);
  });

  test('models.list', async () => {
    const ids = [];
    for await (const m of az.models.list()) ids.push(m.id);
    for (const want of MODELS) assert.ok(ids.includes(want), `${want} ausente: ${ids}`);
  });

  test('API v1: chat e responses', async () => {
    const v1 = new OpenAI({ baseURL: `${BASE_URL}/azure/openai/v1`, apiKey: KEY, maxRetries: 0, timeout: TIMEOUT_MS });
    const r = await v1.chat.completions.create({ model: 'wwz-fast', messages: MESSAGES });
    assert.equal(r.model, 'wwz-fast');
    checkPromptFilter(r);
    const resp = await v1.responses.create({ model: 'wwz-fast', input: 'Say something short.' });
    assert.equal(resp.object, 'response');
    assert.equal(resp.status, 'completed');
    assert.ok(resp.output_text);
    const s = v1.responses.stream({ model: 'wwz-fast', input: 'Say something short.' });
    let deltas = '';
    s.on('response.output_text.delta', (e) => {
      deltas += e.delta;
    });
    const final = await s.finalResponse();
    assert.equal(deltas, final.output_text);
  });
});
