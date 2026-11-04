"""SDK anthropic contra o driver anthropic (spec 018)."""

import anthropic
import pytest

from conftest import BAD_KEY, RL_KEY, anthropic_client, retry_after_seconds

MESSAGES = [{"role": "user", "content": "Say something short."}]
MAX_TOKENS = 2048

WEATHER_TOOL = {
    "name": "get_weather",
    "description": "Weather for a city.",
    "input_schema": {
        "type": "object",
        "properties": {
            "city": {"type": "string"},
            "days": {"type": "integer"},
        },
        "required": ["city"],
    },
}

THINKING = {"type": "enabled", "budget_tokens": 1024}


def text_of(message):
    return "".join(b.text for b in message.content if b.type == "text")


def check_usage(usage):
    assert usage.input_tokens > 0
    assert usage.output_tokens > 0


def test_chat_non_stream(anth):
    m = anth.messages.create(model="wwz-fast", max_tokens=MAX_TOKENS, messages=MESSAGES)
    assert m.type == "message"
    assert m.role == "assistant"
    assert m.id.startswith("msg_")
    assert m.stop_reason == "end_turn"
    assert text_of(m)
    check_usage(m.usage)


def test_chat_stream(anth):
    ref = anth.messages.create(model="wwz-fast", max_tokens=MAX_TOKENS, messages=MESSAGES)
    with anth.messages.stream(model="wwz-fast", max_tokens=MAX_TOKENS, messages=MESSAGES) as s:
        text = "".join(s.text_stream)
        final = s.get_final_message()
    assert text == text_of(final)
    # Mesma requisição com seed fixa: o texto montado é o content.
    assert text == text_of(ref)
    assert final.stop_reason == "end_turn"
    check_usage(final.usage)
    assert final.usage.output_tokens == ref.usage.output_tokens


def check_tool_use(block):
    assert block.type == "tool_use"
    assert block.id.startswith("toolu_")
    assert block.name == "get_weather"
    assert isinstance(block.input, dict)
    assert isinstance(block.input["city"], str)
    if "days" in block.input:
        assert isinstance(block.input["days"], int)


def test_tool_call_non_stream(anth):
    m = anth.messages.create(
        model="wwz-tools", max_tokens=MAX_TOKENS, messages=MESSAGES, tools=[WEATHER_TOOL]
    )
    assert m.stop_reason == "tool_use"
    uses = [b for b in m.content if b.type == "tool_use"]
    assert uses
    for b in uses:
        check_tool_use(b)


def test_tool_call_stream(anth):
    with anth.messages.stream(
        model="wwz-tools", max_tokens=MAX_TOKENS, messages=MESSAGES, tools=[WEATHER_TOOL]
    ) as s:
        partial = sum(1 for e in s if e.type == "input_json")
        final = s.get_final_message()
    assert final.stop_reason == "tool_use"
    uses = [b for b in final.content if b.type == "tool_use"]
    assert uses
    assert partial > 0, "stream sem input_json_delta"
    for b in uses:
        check_tool_use(b)


def test_reasoning_non_stream(anth):
    m = anth.messages.create(
        model="wwz-reason", max_tokens=MAX_TOKENS, messages=MESSAGES, thinking=THINKING
    )
    kinds = [b.type for b in m.content]
    assert kinds[0] == "thinking", kinds
    assert "text" in kinds
    block = m.content[0]
    assert block.thinking
    assert block.signature


def test_reasoning_stream(anth):
    with anth.messages.stream(
        model="wwz-reason", max_tokens=MAX_TOKENS, messages=MESSAGES, thinking=THINKING
    ) as s:
        events = [e.type for e in s]
        final = s.get_final_message()
    assert "thinking" in events
    assert "signature" in events
    assert events.index("thinking") < events.index("text")
    assert final.content[0].type == "thinking"
    assert final.content[0].thinking
    assert final.content[0].signature
    assert text_of(final)


def test_invalid_key():
    client = anthropic_client(key=BAD_KEY)
    with pytest.raises(anthropic.AuthenticationError) as exc:
        client.messages.create(model="wwz-fast", max_tokens=MAX_TOKENS, messages=MESSAGES)
    client.close()
    assert exc.value.status_code == 401
    assert exc.value.body["type"] == "error"
    assert exc.value.body["error"]["type"] == "authentication_error"


def test_rate_limit():
    client = anthropic_client(key=RL_KEY)
    try:
        # rpm: 1 compartilhado com os outros drivers: a segunda chamada
        # sempre bate no limite.
        with pytest.raises(anthropic.RateLimitError) as exc:
            for _ in range(2):
                client.messages.create(model="wwz-rl", max_tokens=MAX_TOKENS, messages=MESSAGES)
    finally:
        client.close()
    assert exc.value.status_code == 429
    assert exc.value.body["error"]["type"] == "rate_limit_error"
    retry_after_seconds(exc.value.response.headers)


def test_models(anth):
    page = anth.models.list()
    ids = [m.id for m in page.data]
    for want in ("wwz-fast", "wwz-tools", "wwz-reason", "wwz-rl"):
        assert want in ids, (want, ids)
    got = anth.models.retrieve("wwz-fast")
    assert got.id == "wwz-fast"
    assert got.type == "model"
