// Helpers dos testes de SDK Node (spec 018, "Testes de SDK").
//
// Rodam contra um wwz já no ar com tests/sdk/wwz.yaml:
//
//   go run ./cmd/wwz serve --config tests/sdk/wwz.yaml
//   WWZ_SDK_BASE_URL=http://127.0.0.1:8000 WWZ_SDK_KEY=sk-wwz-sdk \
//     node --test tests/sdk/node/
//
// Sem WWZ_SDK_BASE_URL e WWZ_SDK_KEY todos os testes saem como "pulado".

import assert from 'node:assert/strict';
import Anthropic from '@anthropic-ai/sdk';
import OpenAI from 'openai';

export const BASE_URL = (process.env.WWZ_SDK_BASE_URL ?? '').replace(/\/+$/, '');
export const KEY = process.env.WWZ_SDK_KEY ?? '';
// Chave com rpm: 1 restrita ao modelo wwz-rl (tests/sdk/wwz.yaml).
export const RL_KEY = process.env.WWZ_SDK_RL_KEY || 'sk-wwz-sdk-rl';
export const BAD_KEY = 'sk-wwz-sdk-invalid-key';

// Opção `skip` de test()/describe(): string com o motivo, ou false.
export const SKIP = BASE_URL && KEY ? false : 'WWZ_SDK_BASE_URL/WWZ_SDK_KEY ausentes';

// Mount de cada driver em tests/sdk/wwz.yaml, na forma de baseURL do SDK.
export const OPENAI_COMPAT = {
  opencode: '/opencode/zen/go/v1',
  openai: '/openai/v1',
  openrouter: '/openrouter/api/v1',
};
export const ANTHROPIC_MOUNT = '/anthropic';

export const MODELS = ['wwz-fast', 'wwz-tools', 'wwz-reason', 'wwz-rl'];
export const MESSAGES = [{ role: 'user', content: 'Say something short.' }];

const TIMEOUT_MS = 30_000;

export function openaiClient(driver, key) {
  return new OpenAI({
    baseURL: BASE_URL + OPENAI_COMPAT[driver],
    apiKey: key || KEY,
    maxRetries: 0,
    timeout: TIMEOUT_MS,
    // O OpenCode exige a sessão, como o real (ADR 0059).
    defaultHeaders: driver === 'opencode' ? { 'x-opencode-session': 'ses_wwz_sdk' } : undefined,
  });
}

export function anthropicClient(key) {
  return new Anthropic({
    baseURL: BASE_URL + ANTHROPIC_MOUNT,
    apiKey: key || KEY,
    maxRetries: 0,
    timeout: TIMEOUT_MS,
  });
}

// Lê retry-after (segundos inteiros ou fracionários) dos headers do erro.
export function retryAfterSeconds(headers) {
  assert.ok(headers, '429 sem headers');
  const value = headers.get('retry-after');
  assert.ok(value != null, `429 sem retry-after: ${JSON.stringify(Object.fromEntries(headers))}`);
  const seconds = Number(value);
  assert.ok(Number.isFinite(seconds) && seconds > 0, `retry-after não positivo: ${value}`);
  return seconds;
}

// Faz até `n` chamadas e devolve o primeiro erro lançado (ou falha).
export async function firstError(n, call) {
  for (let i = 0; i < n; i++) {
    try {
      await call();
    } catch (err) {
      return err;
    }
  }
  return assert.fail(`nenhum erro em ${n} chamadas`);
}
