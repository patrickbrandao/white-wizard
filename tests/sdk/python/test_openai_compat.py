"""SDK openai contra os drivers openai, openrouter e opencode (spec 018)."""

import json

import openai
import pytest

from conftest import BAD_KEY, RL_KEY, openai_client, retry_after_seconds

MESSAGES = [{"role": "user", "content": "Say something short."}]

WEATHER_TOOL = {
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "Weather for a city.",
        "parameters": {
            "type": "object",
            "properties": {
                "city": {"type": "string"},
                "days": {"type": "integer"},
            },
            "required": ["city"],
        },
    },
}


def extra(obj):
    """Campos fora do modelo pydantic do SDK (dialetos de raciocínio)."""
    return getattr(obj, "model_extra", None) or {}


def response_model(driver, model_id):
    """model da resposta: o slug no openrouter (ADR 0038), o id nos demais."""
    if driver == "openrouter" and "/" not in model_id:
        return "white-wizard/" + model_id
    return model_id


def check_usage(usage):
    assert usage is not None
    assert usage.prompt_tokens > 0
    assert usage.completion_tokens > 0
    assert usage.total_tokens == usage.prompt_tokens + usage.completion_tokens


def test_chat_non_stream(oai, driver):
    r = oai.chat.completions.create(model="wwz-fast", messages=MESSAGES)
    assert r.object == "chat.completion"
    assert r.model == response_model(driver, "wwz-fast")
    assert r.id
    choice = r.choices[0]
    assert choice.message.role == "assistant"
    assert choice.message.content
    assert choice.finish_reason == "stop"
    check_usage(r.usage)


def test_chat_stream(oai, driver):
    ref = oai.chat.completions.create(model="wwz-fast", messages=MESSAGES)
    stream = oai.chat.completions.create(
        model="wwz-fast",
        messages=MESSAGES,
        stream=True,
        stream_options={"include_usage": True},
    )
    text, usage, finish, ids = "", None, None, set()
    for chunk in stream:
        assert chunk.object == "chat.completion.chunk"
        assert chunk.model == response_model(driver, "wwz-fast")
        ids.add(chunk.id)
        if chunk.choices:
            delta = chunk.choices[0].delta
            if delta.content:
                text += delta.content
            if chunk.choices[0].finish_reason:
                finish = chunk.choices[0].finish_reason
        if chunk.usage:
            usage = chunk.usage
    assert len(ids) == 1, f"ids diferentes no mesmo stream: {ids}"
    assert finish == "stop"
    # Mesma requisição com seed fixa: o texto montado é o content.
    assert text == ref.choices[0].message.content
    check_usage(usage)
    assert usage.completion_tokens == ref.usage.completion_tokens


def test_chat_stream_helper(oai):
    with oai.chat.completions.stream(model="wwz-fast", messages=MESSAGES) as s:
        deltas = "".join(e.delta for e in s if e.type == "content.delta")
        final = s.get_final_completion()
    assert deltas
    assert final.choices[0].message.content == deltas


def check_tool_call(call):
    assert call.type == "function"
    assert call.id
    assert call.function.name == "get_weather"
    args = json.loads(call.function.arguments)
    assert isinstance(args, dict)
    assert isinstance(args["city"], str)
    if "days" in args:
        assert isinstance(args["days"], int)


def test_tool_call_non_stream(oai):
    r = oai.chat.completions.create(model="wwz-tools", messages=MESSAGES, tools=[WEATHER_TOOL])
    choice = r.choices[0]
    assert choice.finish_reason == "tool_calls"
    assert choice.message.tool_calls
    for call in choice.message.tool_calls:
        check_tool_call(call)
    check_usage(r.usage)


def test_tool_call_stream(oai):
    with oai.chat.completions.stream(
        model="wwz-tools", messages=MESSAGES, tools=[WEATHER_TOOL]
    ) as s:
        done = [e for e in s if e.type == "tool_calls.function.arguments.done"]
        final = s.get_final_completion()
    assert done, "stream sem tool_calls.function.arguments.done"
    choice = final.choices[0]
    assert choice.finish_reason == "tool_calls"
    for call in choice.message.tool_calls:
        check_tool_call(call)
    # Acumulado pelo SDK = argumentos completos de cada evento done.
    assert [json.loads(e.arguments) for e in done] == [
        json.loads(c.function.arguments) for c in choice.message.tool_calls
    ]


def reasoning_kwargs(driver):
    if driver == "openrouter":
        return {"extra_body": {"reasoning": {"effort": "low"}}}
    return {"reasoning_effort": "low"}


def test_reasoning_non_stream(oai, driver):
    r = oai.chat.completions.create(model="wwz-reason", messages=MESSAGES, **reasoning_kwargs(driver))
    msg = r.choices[0].message
    fields = extra(msg)
    assert msg.content
    assert r.usage.completion_tokens_details.reasoning_tokens > 0
    if driver == "openai":
        # ADR 0022: Chat Completions real esconde o texto do raciocínio.
        assert "reasoning_content" not in fields
        assert "reasoning" not in fields
    elif driver == "opencode":
        assert fields.get("reasoning_content")
        assert "reasoning" not in fields
    else:
        assert fields.get("reasoning")
        assert "reasoning_content" not in fields


def test_reasoning_stream(oai, driver):
    stream = oai.chat.completions.create(
        model="wwz-reason",
        messages=MESSAGES,
        stream=True,
        stream_options={"include_usage": True},
        **reasoning_kwargs(driver),
    )
    order, reasoning, usage = [], "", None
    for chunk in stream:
        if chunk.usage:
            usage = chunk.usage
        if not chunk.choices:
            continue
        delta = chunk.choices[0].delta
        fields = extra(delta)
        for name in ("reasoning_content", "reasoning"):
            if fields.get(name):
                reasoning += fields[name]
                order.append(name)
        if delta.content:
            order.append("content")
    assert usage is not None
    assert usage.completion_tokens_details.reasoning_tokens > 0
    assert "content" in order
    if driver == "openai":
        assert set(order) == {"content"}
    else:
        want = "reasoning_content" if driver == "opencode" else "reasoning"
        assert reasoning
        assert set(order) == {want, "content"}
        # Raciocínio vem antes do primeiro conteúdo visível.
        assert order.index("content") > order.index(want)
        assert want not in order[order.index("content") :]


def test_openrouter_reasoning_details():
    client = openai_client("openrouter")
    try:
        r = client.chat.completions.create(
            model="wwz-reason", messages=MESSAGES, **reasoning_kwargs("openrouter")
        )
        stream = client.chat.completions.create(
            model="wwz-reason", messages=MESSAGES, stream=True, **reasoning_kwargs("openrouter")
        )
        streamed = [
            d
            for chunk in stream
            if chunk.choices
            for d in extra(chunk.choices[0].delta).get("reasoning_details") or []
        ]
    finally:
        client.close()
    message = extra(r.choices[0].message)
    details = message.get("reasoning_details")
    assert isinstance(details, list) and details, "reasoning_details[] ausente (spec 006)"
    assert streamed, "delta.reasoning_details ausente no stream (spec 006)"
    assert "".join(d.get("text", "") for d in details) == message.get("reasoning")
    for d in details + streamed:
        assert d.get("type", "").startswith("reasoning.")


def test_invalid_key(driver):
    client = openai_client(driver, key=BAD_KEY)
    with pytest.raises(openai.AuthenticationError) as exc:
        client.chat.completions.create(model="wwz-fast", messages=MESSAGES)
    assert exc.value.status_code == 401
    client.close()


def test_rate_limit(driver):
    client = openai_client(driver, key=RL_KEY)
    try:
        # rpm: 1 compartilhado entre os drivers: a primeira chamada pode
        # passar ou já bater no limite; a segunda sempre bate.
        with pytest.raises(openai.RateLimitError) as exc:
            for _ in range(2):
                client.chat.completions.create(model="wwz-rl", messages=MESSAGES)
    finally:
        client.close()
    assert exc.value.status_code == 429
    retry_after_seconds(exc.value.response.headers)


def test_models(oai):
    page = oai.models.list()
    ids = [m.id for m in page.data]
    for want in ("wwz-fast", "wwz-tools", "wwz-reason", "wwz-rl"):
        assert any(i == want or i.endswith("/" + want) for i in ids), (want, ids)


def test_listed_id_usable(oai, driver):
    """Listar e usar o id listado funciona; a resposta devolve esse id (ADR 0038)."""
    listed = [m.id for m in oai.models.list().data if m.id.split("/")[-1] == "wwz-fast"]
    assert listed, "wwz-fast ausente na listagem"
    r = oai.chat.completions.create(model=listed[0], messages=MESSAGES)
    assert r.model == listed[0]
    for chunk in oai.chat.completions.create(model=listed[0], messages=MESSAGES, stream=True):
        assert chunk.model == listed[0]


def test_openrouter_model_field():
    """OpenRouter resolve o model como o real (task 163): slug, nome sem autor
    e qualquer caixa resolvem para o slug; nome desconhecido, com ou sem
    autor, é 400 "<model> is not a valid model ID"."""
    client = openai_client("openrouter")
    try:
        for asked in ("white-wizard/wwz-fast", "wwz-fast", "WWZ-Fast"):
            r = client.chat.completions.create(model=asked, messages=MESSAGES)
            assert r.model == "white-wizard/wwz-fast", asked
        for asked in ("white-wizard/nope", "nope", "x/wwz-fast"):
            with pytest.raises(openai.BadRequestError) as exc:
                client.chat.completions.create(model=asked, messages=MESSAGES)
            assert exc.value.status_code == 400
            assert exc.value.body == {"message": asked + " is not a valid model ID", "code": 400}
    finally:
        client.close()
