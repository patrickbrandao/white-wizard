"""SDK google-genai contra o driver gemini (spec 018, "Testes de SDK"; ADR 0043).

O cliente monta <base>/gemini/v1beta/..., manda x-goog-api-key e pede
?alt=sse no stream (spec 006 16.5).
"""

import pytest
from google import genai
from google.genai import errors, types

from conftest import BAD_KEY, BASE_URL, KEY, RL_KEY

GEMINI_MOUNT = "/gemini"
PROMPT = "Say something short."
WEATHER = types.Tool(
    function_declarations=[
        types.FunctionDeclaration(
            name="get_weather",
            description="Weather of a city",
            parameters=types.Schema(
                type="OBJECT",
                properties={"city": types.Schema(type="STRING")},
                required=["city"],
            ),
        )
    ]
)


def gemini_client(key=None):
    return genai.Client(
        api_key=key or KEY,
        http_options=types.HttpOptions(base_url=BASE_URL + GEMINI_MOUNT, timeout=30_000),
    )


@pytest.fixture
def gem():
    client = gemini_client()
    yield client
    client.close()


def test_generate_content(gem):
    r = gem.models.generate_content(model="wwz-fast", contents=PROMPT)
    assert r.text
    assert r.model_version == "wwz-fast"
    assert r.response_id
    cand = r.candidates[0]
    assert cand.finish_reason == types.FinishReason.STOP
    assert cand.content.role == "model"
    u = r.usage_metadata
    assert u.prompt_token_count > 0
    assert u.candidates_token_count > 0
    assert u.total_token_count == u.prompt_token_count + u.candidates_token_count


def test_stream_matches(gem):
    full = gem.models.generate_content(model="wwz-fast", contents=PROMPT)
    chunks = list(gem.models.generate_content_stream(model="wwz-fast", contents=PROMPT))
    assert len(chunks) >= 2
    text = "".join(c.text or "" for c in chunks)
    assert text == full.text
    ids = {c.response_id for c in chunks}
    assert len(ids) == 1
    assert chunks[-1].candidates[0].finish_reason == types.FinishReason.STOP
    assert all(c.candidates[0].finish_reason is None for c in chunks[:-1])
    assert chunks[-1].usage_metadata.candidates_token_count == full.usage_metadata.candidates_token_count


def test_function_call(gem):
    cfg = types.GenerateContentConfig(
        tools=[WEATHER],
        tool_config=types.ToolConfig(function_calling_config=types.FunctionCallingConfig(mode="ANY")),
    )
    r = gem.models.generate_content(model="wwz-tools", contents="Weather in Paris?", config=cfg)
    calls = r.function_calls
    assert calls and calls[0].name == "get_weather"
    assert isinstance(calls[0].args, dict)
    assert calls[0].id.startswith("call_")
    part = r.candidates[0].content.parts[[p.function_call is not None for p in r.candidates[0].content.parts].index(True)]
    assert part.thought_signature
    stream = list(gem.models.generate_content_stream(model="wwz-tools", contents="Weather in Paris?", config=cfg))
    streamed = [p.function_call for c in stream for p in (c.candidates[0].content.parts or []) if p.function_call]
    assert streamed and streamed[0].name == "get_weather"


def test_thoughts(gem):
    cfg = types.GenerateContentConfig(thinking_config=types.ThinkingConfig(include_thoughts=True, thinking_budget=64))
    r = gem.models.generate_content(model="wwz-reason", contents=PROMPT, config=cfg)
    parts = r.candidates[0].content.parts
    assert parts[0].thought is True and parts[0].text
    assert r.usage_metadata.thoughts_token_count > 0
    hidden = gem.models.generate_content(
        model="wwz-reason", contents=PROMPT,
        config=types.GenerateContentConfig(thinking_config=types.ThinkingConfig(thinking_budget=64)),
    )
    assert not any(p.thought for p in hidden.candidates[0].content.parts)
    assert hidden.usage_metadata.thoughts_token_count > 0


def test_count_tokens(gem):
    r = gem.models.count_tokens(model="wwz-fast", contents=PROMPT)
    assert r.total_tokens > 0


def test_models(gem):
    names = [m.name for m in gem.models.list()]
    assert "models/wwz-fast" in names
    m = gem.models.get(model="wwz-fast")
    assert m.name == "models/wwz-fast"
    assert "generateContent" in m.supported_actions


def test_invalid_key():
    client = gemini_client(BAD_KEY)
    with pytest.raises(errors.ClientError) as exc:
        client.models.generate_content(model="wwz-fast", contents=PROMPT)
    assert exc.value.code == 400
    assert exc.value.status == "INVALID_ARGUMENT"
    assert "API key not valid" in exc.value.message


def test_unknown_model(gem):
    with pytest.raises(errors.ClientError) as exc:
        gem.models.generate_content(model="wwz-nope", contents=PROMPT)
    assert exc.value.code == 404
    assert exc.value.status == "NOT_FOUND"


def test_rate_limit():
    client = gemini_client(RL_KEY)
    with pytest.raises(errors.ClientError) as exc:
        for _ in range(2):
            client.models.generate_content(model="wwz-rl", contents=PROMPT)
    assert exc.value.code == 429
    assert exc.value.status == "RESOURCE_EXHAUSTED"


def blocked_config():
    # x-wwz-return-error: content_filter bloqueia o prompt (task 382).
    return types.GenerateContentConfig(
        http_options=types.HttpOptions(headers={"x-wwz-return-error": "content_filter"})
    )


def test_prompt_blocked(gem):
    r = gem.models.generate_content(model="wwz-fast", contents=PROMPT, config=blocked_config())
    assert r.prompt_feedback.block_reason == types.BlockedReason.SAFETY
    assert not r.candidates
    assert r.usage_metadata.prompt_token_count > 0
    assert r.model_version == "wwz-fast"


def test_prompt_blocked_stream(gem):
    chunks = list(gem.models.generate_content_stream(model="wwz-fast", contents=PROMPT, config=blocked_config()))
    assert len(chunks) == 1
    assert chunks[0].prompt_feedback.block_reason == types.BlockedReason.SAFETY
    assert not chunks[0].candidates


def test_cached_content(gem):
    with pytest.raises(errors.ClientError) as exc:
        gem.caches.create(model="wwz-fast", config=types.CreateCachedContentConfig(contents=["hi"]))
    assert exc.value.code == 400
    long = " ".join(f"line {i} of a long cached document about simulated providers." for i in range(400))
    cache = gem.caches.create(
        model="wwz-fast",
        config=types.CreateCachedContentConfig(contents=[long], display_name="sdk", ttl="300s"),
    )
    assert cache.name.startswith("cachedContents/")
    assert cache.usage_metadata.total_token_count >= 1024
    assert gem.caches.get(name=cache.name).display_name == "sdk"
    r = gem.models.generate_content(
        model="wwz-fast", contents=PROMPT, config=types.GenerateContentConfig(cached_content=cache.name),
    )
    assert r.usage_metadata.cached_content_token_count == cache.usage_metadata.total_token_count
    assert r.usage_metadata.prompt_token_count > r.usage_metadata.cached_content_token_count
    gem.caches.update(name=cache.name, config=types.UpdateCachedContentConfig(ttl="120s"))
    gem.caches.delete(name=cache.name)
    with pytest.raises(errors.ClientError) as exc:
        gem.caches.get(name=cache.name)
    assert exc.value.code == 403
