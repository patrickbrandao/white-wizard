"""SDK openai, Responses API, contra openai, openrouter e opencode (spec 004
"Responses API", ADRs 0054 e 0055; task 239).

O openai guarda a resposta e aceita previous_response_id (o motor resolve a
cadeia); os gateways não guardam nada e recusam previous_response_id e
store: true com 400, como os reais. Neles a conversa encadeada manda os
itens anteriores no input. O azure (API v1) está em test_azure.py.
"""

import json

import openai
import pytest

from conftest import BAD_KEY

PROMPT = "Say something short."

WEATHER_TOOL = {
    "type": "function",
    "name": "get_weather",
    "description": "Weather for a city.",
    "parameters": {
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"],
    },
}

STATEFUL = {"openai"}


def model_name(driver, model_id):
    """model da resposta: o slug no openrouter (ADR 0038), o id nos demais."""
    if driver == "openrouter":
        return "white-wizard/" + model_id
    return model_id


def check_usage(usage):
    assert usage is not None
    assert usage.input_tokens > 0
    assert usage.output_tokens > 0
    assert usage.total_tokens == usage.input_tokens + usage.output_tokens
    assert usage.input_tokens_details.cached_tokens >= 0
    assert usage.output_tokens_details.reasoning_tokens >= 0


def test_create_non_stream(oai, driver):
    r = oai.responses.create(model="wwz-fast", input=PROMPT, instructions="Be brief.")
    assert r.object == "response"
    assert r.id.startswith("gen-" if driver == "openrouter" else "resp_")
    assert r.status == "completed"
    assert r.model == model_name(driver, "wwz-fast")
    assert r.output_text
    msgs = [o for o in r.output if o.type == "message"]
    assert len(msgs) == 1 and msgs[0].role == "assistant"
    assert msgs[0].content[0].type == "output_text"
    check_usage(r.usage)


def test_create_stream(oai, driver):
    ref = oai.responses.create(model="wwz-fast", input=PROMPT)
    stream = oai.responses.create(model="wwz-fast", input=PROMPT, stream=True)
    types, text, final, seq = [], "", None, []
    for ev in stream:
        types.append(ev.type)
        seq.append(ev.sequence_number)
        if ev.type == "response.output_text.delta":
            text += ev.delta
        if ev.type == "response.completed":
            final = ev.response
    assert types[0] == "response.created"
    assert types[-1] == "response.completed"
    assert seq == list(range(len(seq)))
    assert final is not None
    # Mesma requisição com seed fixa: o texto montado é o da resposta.
    assert text == final.output_text == ref.output_text
    check_usage(final.usage)
    assert final.usage.output_tokens == ref.usage.output_tokens


def test_stream_helper(oai):
    with oai.responses.stream(model="wwz-fast", input=PROMPT) as s:
        deltas = "".join(e.delta for e in s if e.type == "response.output_text.delta")
        final = s.get_final_response()
    assert deltas
    assert final.output_text == deltas


def test_incomplete(oai):
    r = oai.responses.create(model="wwz-fast", input=PROMPT, max_output_tokens=16)
    assert r.status == "incomplete"
    assert r.incomplete_details.reason == "max_output_tokens"
    assert r.usage.output_tokens == 16


def function_calls(r):
    return [o for o in r.output if o.type == "function_call"]


def check_call(call):
    assert call.call_id
    assert call.name == "get_weather"
    args = json.loads(call.arguments)
    assert isinstance(args["city"], str)


def test_tool_non_stream(oai):
    r = oai.responses.create(model="wwz-tools", input=PROMPT, tools=[WEATHER_TOOL])
    calls = function_calls(r)
    assert calls
    for c in calls:
        check_call(c)
    assert r.tools[0].name == "get_weather"


def test_tool_stream(oai):
    with oai.responses.stream(model="wwz-tools", input=PROMPT, tools=[WEATHER_TOOL]) as s:
        done = [e for e in s if e.type == "response.function_call_arguments.done"]
        final = s.get_final_response()
    calls = function_calls(final)
    assert done and calls
    for c in calls:
        check_call(c)
    assert [json.loads(e.arguments) for e in done] == [json.loads(c.arguments) for c in calls]


def test_chained_with_items(oai):
    """Encadeamento sem estado: os itens anteriores voltam no input."""
    r1 = oai.responses.create(model="wwz-tools", input=PROMPT, tools=[WEATHER_TOOL], store=False)
    call = function_calls(r1)[0]
    items = [{"role": "user", "content": PROMPT}, *[o.model_dump(exclude_none=True) for o in r1.output]]
    items.append({"type": "function_call_output", "call_id": call.call_id, "output": "22C, sunny"})
    r2 = oai.responses.create(model="wwz-fast", input=items, store=False)
    assert r2.status == "completed"
    assert r2.output_text
    # O histórico entra no prompt: mais entrada que a mesma pergunta sozinha.
    alone = oai.responses.create(model="wwz-fast", input=PROMPT, store=False)
    assert r2.usage.input_tokens > alone.usage.input_tokens


def test_previous_response_id(oai, driver):
    r1 = oai.responses.create(model="wwz-fast", input=PROMPT, store=False if driver not in STATEFUL else True)
    if driver not in STATEFUL:
        # O gateway não guarda respostas: 400 (ADR 0055).
        with pytest.raises(openai.BadRequestError) as e:
            oai.responses.create(model="wwz-fast", input="And now?", previous_response_id=r1.id)
        assert e.value.status_code == 400
        assert "previous_response_id" in str(e.value)
        return
    r2 = oai.responses.create(model="wwz-fast", input="And now?", previous_response_id=r1.id)
    assert r2.previous_response_id == r1.id
    assert r2.status == "completed"
    # A cadeia entra no prompt: mais tokens de entrada que a primeira volta.
    assert r2.usage.input_tokens > r1.usage.input_tokens
    with oai.responses.stream(model="wwz-fast", input="Once more.", previous_response_id=r2.id) as s:
        final = s.get_final_response()
    assert final.previous_response_id == r2.id
    assert final.usage.input_tokens > r2.usage.input_tokens


def test_previous_response_tool_output(oai, driver):
    """Volta de tool pela cadeia: só function_call_output no input."""
    if driver not in STATEFUL:
        pytest.skip("gateway sem estado: coberto por test_chained_with_items")
    r1 = oai.responses.create(model="wwz-tools", input=PROMPT, tools=[WEATHER_TOOL])
    call = function_calls(r1)[0]
    r2 = oai.responses.create(
        model="wwz-fast",
        previous_response_id=r1.id,
        input=[{"type": "function_call_output", "call_id": call.call_id, "output": "22C"}],
    )
    assert r2.status == "completed"
    assert r2.output_text


def test_previous_response_not_found(oai, driver):
    missing = "resp_" + "0" * 50
    with pytest.raises(openai.BadRequestError) as e:
        oai.responses.create(model="wwz-fast", input="x", previous_response_id=missing)
    assert e.value.status_code == 400
    if driver in STATEFUL:
        assert e.value.code == "previous_response_not_found"
        assert e.value.param == "previous_response_id"


def test_store_true(oai, driver):
    if driver in STATEFUL:
        r = oai.responses.create(model="wwz-fast", input=PROMPT, store=True)
        assert r.store is True
        return
    with pytest.raises(openai.BadRequestError) as e:
        oai.responses.create(model="wwz-fast", input=PROMPT, store=True)
    assert e.value.status_code == 400


def test_invalid_key(driver):
    from conftest import openai_client

    client = openai_client(driver, key=BAD_KEY)
    try:
        with pytest.raises(openai.AuthenticationError) as e:
            client.responses.create(model="wwz-fast", input=PROMPT)
        assert e.value.status_code == 401
    finally:
        client.close()
