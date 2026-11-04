// SDK @google/genai (Node) contra o driver gemini (spec 018; ADR 0043).
//
// O cliente monta <base>/gemini/v1beta/..., manda x-goog-api-key e pede
// ?alt=sse no stream (spec 006 16.5).

import assert from 'node:assert/strict';
import { describe, test } from 'node:test';
import { ApiError, FunctionCallingConfigMode, GoogleGenAI } from '@google/genai';
import { BAD_KEY, BASE_URL, KEY, RL_KEY, SKIP, firstError } from './helpers.mjs';

const GEMINI_MOUNT = '/gemini';
const PROMPT = 'Say something short.';
const WEATHER = {
  functionDeclarations: [
    {
      name: 'get_weather',
      description: 'Weather of a city',
      parameters: { type: 'OBJECT', properties: { city: { type: 'STRING' } }, required: ['city'] },
    },
  ],
};

function geminiClient(key) {
  return new GoogleGenAI({ apiKey: key || KEY, httpOptions: { baseUrl: BASE_URL + GEMINI_MOUNT, timeout: 30_000 } });
}

describe('gemini (@google/genai)', { skip: SKIP }, () => {
  test('generateContent', async () => {
    const r = await geminiClient().models.generateContent({ model: 'wwz-fast', contents: PROMPT });
    assert.ok(r.text);
    assert.equal(r.modelVersion, 'wwz-fast');
    assert.ok(r.responseId);
    assert.equal(r.candidates[0].finishReason, 'STOP');
    const u = r.usageMetadata;
    assert.ok(u.promptTokenCount > 0 && u.candidatesTokenCount > 0);
    assert.equal(u.totalTokenCount, u.promptTokenCount + u.candidatesTokenCount);
  });

  test('generateContentStream monta o mesmo texto', async () => {
    const ai = geminiClient();
    const full = await ai.models.generateContent({ model: 'wwz-fast', contents: PROMPT });
    const chunks = [];
    for await (const c of await ai.models.generateContentStream({ model: 'wwz-fast', contents: PROMPT })) {
      chunks.push(c);
    }
    assert.ok(chunks.length >= 2);
    assert.equal(chunks.map((c) => c.text ?? '').join(''), full.text);
    assert.equal(new Set(chunks.map((c) => c.responseId)).size, 1);
    assert.equal(chunks.at(-1).candidates[0].finishReason, 'STOP');
  });

  test('functionCall com e sem stream', async () => {
    const ai = geminiClient();
    const config = {
      tools: [WEATHER],
      toolConfig: { functionCallingConfig: { mode: FunctionCallingConfigMode.ANY } },
    };
    const r = await ai.models.generateContent({ model: 'wwz-tools', contents: 'Weather in Paris?', config });
    const calls = r.functionCalls;
    assert.ok(calls?.length);
    assert.equal(calls[0].name, 'get_weather');
    assert.equal(typeof calls[0].args, 'object');
    assert.match(calls[0].id, /^call_\d{6}$/);
    const streamed = [];
    for await (const c of await ai.models.generateContentStream({ model: 'wwz-tools', contents: 'Weather in Paris?', config })) {
      streamed.push(...(c.functionCalls ?? []));
    }
    assert.equal(streamed[0]?.name, 'get_weather');
  });

  test('thoughts com includeThoughts', async () => {
    const r = await geminiClient().models.generateContent({
      model: 'wwz-reason',
      contents: PROMPT,
      config: { thinkingConfig: { includeThoughts: true, thinkingBudget: 64 } },
    });
    const parts = r.candidates[0].content.parts;
    assert.equal(parts[0].thought, true);
    assert.ok(r.usageMetadata.thoughtsTokenCount > 0);
  });

  test('countTokens', async () => {
    const r = await geminiClient().models.countTokens({ model: 'wwz-fast', contents: PROMPT });
    assert.ok(r.totalTokens > 0);
  });

  test('models.list e models.get', async () => {
    const ai = geminiClient();
    const names = [];
    for await (const m of await ai.models.list()) names.push(m.name);
    assert.ok(names.includes('models/wwz-fast'));
    const m = await ai.models.get({ model: 'wwz-fast' });
    assert.equal(m.name, 'models/wwz-fast');
  });

  test('chave inválida: 400 INVALID_ARGUMENT', async () => {
    const err = await firstError(1, () => geminiClient(BAD_KEY).models.generateContent({ model: 'wwz-fast', contents: PROMPT }));
    assert.ok(err instanceof ApiError);
    assert.equal(err.status, 400);
    assert.match(err.message, /API key not valid/);
  });

  test('modelo desconhecido: 404', async () => {
    const err = await firstError(1, () => geminiClient().models.generateContent({ model: 'wwz-nope', contents: PROMPT }));
    assert.equal(err.status, 404);
  });

  test('429 RESOURCE_EXHAUSTED', async () => {
    const ai = geminiClient(RL_KEY);
    const err = await firstError(2, () => ai.models.generateContent({ model: 'wwz-rl', contents: PROMPT }));
    assert.equal(err.status, 429);
    assert.match(err.message, /RESOURCE_EXHAUSTED/);
  });

  test('prompt bloqueado: promptFeedback.blockReason sem candidatos', async () => {
    // x-wwz-return-error: content_filter bloqueia o prompt (task 382).
    const config = { httpOptions: { headers: { 'x-wwz-return-error': 'content_filter' } } };
    const ai = geminiClient();
    const r = await ai.models.generateContent({ model: 'wwz-fast', contents: PROMPT, config });
    assert.equal(r.promptFeedback.blockReason, 'SAFETY');
    assert.ok(!r.candidates || r.candidates.length === 0);
    assert.ok(r.usageMetadata.promptTokenCount > 0);
    const chunks = [];
    for await (const c of await ai.models.generateContentStream({ model: 'wwz-fast', contents: PROMPT, config })) chunks.push(c);
    assert.equal(chunks.length, 1);
    assert.equal(chunks[0].promptFeedback.blockReason, 'SAFETY');
  });

  test('cachedContents', async () => {
    const ai = geminiClient();
    const small = await firstError(1, () => ai.caches.create({ model: 'wwz-fast', config: { contents: ['hi'] } }));
    assert.equal(small.status, 400);
    const long = Array.from({ length: 400 }, (_, i) => `line ${i} of a long cached document about simulated providers.`).join(' ');
    const cache = await ai.caches.create({ model: 'wwz-fast', config: { contents: [long], displayName: 'sdk', ttl: '300s' } });
    assert.match(cache.name, /^cachedContents\//);
    const r = await ai.models.generateContent({ model: 'wwz-fast', contents: PROMPT, config: { cachedContent: cache.name } });
    assert.equal(r.usageMetadata.cachedContentTokenCount, cache.usageMetadata.totalTokenCount);
    await ai.caches.delete({ name: cache.name });
    const gone = await firstError(1, () => ai.caches.get({ name: cache.name }));
    assert.equal(gone.status, 403);
  });
});
