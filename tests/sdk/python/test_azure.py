"""SDK openai com AzureOpenAI contra o driver azure (spec 018, ADR 0043).

AzureOpenAI(azure_endpoint=<base>/azure) chama as rotas por deployment
(/openai/deployments/{d}/...?api-version=) com api-key; OpenAI(base_url=
<base>/azure/openai/v1/) chama a API v1 com Bearer (spec 006, 16.6).
"""

import json

import openai
import pytest

from conftest import BAD_KEY, BASE_URL, KEY, RL_KEY, TIMEOUT

MESSAGES = [{"role": "user", "content": "Say something short."}]
API_VERSION = "2024-10-21"

WEATHER_TOOL = {
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "Weather for a city.",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"],
        },
    },
}


def azure_client(key=None):
    return openai.AzureOpenAI(
        azure_endpoint=BASE_URL + "/azure",
        api_key=key or KEY,
        api_version=API_VERSION,
        max_retries=0,
        timeout=TIMEOUT,
    )


def v1_client():
    return openai.OpenAI(base_url=BASE_URL + "/azure/openai/v1/", api_key=KEY, max_retries=0, timeout=TIMEOUT)


@pytest.fixture
def az():
    client = azure_client()
    yield client
    client.close()


def extra(obj):
    return getattr(obj, "model_extra", None) or {}


def check_usage(usage):
    assert usage is not None
    assert usage.prompt_tokens > 0
    assert usage.completion_tokens > 0
    assert usage.total_tokens == usage.prompt_tokens + usage.completion_tokens


def check_prompt_filter(obj):
    pfr = extra(obj).get("prompt_filter_results")
    assert pfr and pfr[0]["prompt_index"] == 0, extra(obj)
    for cat in ("hate", "self_harm", "sexual", "violence"):
        assert pfr[0]["content_filter_results"][cat] == {"filtered": False, "severity": "safe"}


def test_chat_non_stream(az):
    r = az.chat.completions.create(model="wwz-fast", messages=MESSAGES)
    assert r.object == "chat.completion"
    # O model da resposta é o id do modelo, não o nome do deployment.
    assert r.model == "wwz-fast"
    assert r.id.startswith("chatcmpl-")
    choice = r.choices[0]
    assert choice.message.content
    assert choice.finish_reason == "stop"
    check_usage(r.usage)
    check_prompt_filter(r)
    assert extra(choice)["content_filter_results"]["violence"]["severity"] == "safe"


def test_chat_stream(az):
    ref = az.chat.completions.create(model="wwz-fast", messages=MESSAGES)
    stream = az.chat.completions.create(
        model="wwz-fast", messages=MESSAGES, stream=True, stream_options={"include_usage": True}
    )
    chunks = list(stream)
    # Primeiro evento: só prompt_filter_results, sem choices (spec 006, 16.6).
    first = chunks[0]
    assert first.choices == [] and first.id == "" and first.model == ""
    check_prompt_filter(first)
    text, usage, finish = "", None, None
    for chunk in chunks[1:]:
        assert chunk.object == "chat.completion.chunk"
        assert chunk.model == "wwz-fast"
        if chunk.choices:
            if chunk.choices[0].delta.content:
                text += chunk.choices[0].delta.content
            if chunk.choices[0].finish_reason:
                finish = chunk.choices[0].finish_reason
        if chunk.usage:
            usage = chunk.usage
    assert finish == "stop"
    assert text == ref.choices[0].message.content
    check_usage(usage)


def test_chat_stream_helper(az):
    with az.chat.completions.stream(model="wwz-fast", messages=MESSAGES) as s:
        deltas = "".join(e.delta for e in s if e.type == "content.delta")
        final = s.get_final_completion()
    assert deltas
    assert final.choices[0].message.content == deltas


def test_tool_call(az):
    r = az.chat.completions.create(model="wwz-tools", messages=MESSAGES, tools=[WEATHER_TOOL])
    choice = r.choices[0]
    assert choice.finish_reason == "tool_calls"
    for call in choice.message.tool_calls:
        assert call.function.name == "get_weather"
        assert isinstance(json.loads(call.function.arguments)["city"], str)
    with az.chat.completions.stream(model="wwz-tools", messages=MESSAGES, tools=[WEATHER_TOOL]) as s:
        done = [e for e in s if e.type == "tool_calls.function.arguments.done"]
        final = s.get_final_completion()
    assert done
    assert final.choices[0].finish_reason == "tool_calls"


def test_reasoning_hidden(az):
    r = az.chat.completions.create(model="wwz-reason", messages=MESSAGES, reasoning_effort="low")
    assert r.choices[0].message.content
    assert r.usage.completion_tokens_details.reasoning_tokens > 0
    assert "reasoning_content" not in extra(r.choices[0].message)


def test_completions_legacy(az):
    r = az.completions.create(model="wwz-fast", prompt="Say something", max_tokens=8)
    assert r.object == "text_completion"
    assert r.id.startswith("cmpl-")
    assert r.choices[0].text


def test_invalid_key():
    client = azure_client(key=BAD_KEY)
    try:
        with pytest.raises(openai.AuthenticationError) as exc:
            client.chat.completions.create(model="wwz-fast", messages=MESSAGES)
    finally:
        client.close()
    assert exc.value.status_code == 401
    assert exc.value.code == "401"


def test_deployment_not_found(az):
    with pytest.raises(openai.NotFoundError) as exc:
        az.chat.completions.create(model="no-such-deployment", messages=MESSAGES)
    assert exc.value.code == "DeploymentNotFound"


def test_unknown_argument(az):
    with pytest.raises(openai.BadRequestError) as exc:
        az.chat.completions.create(model="wwz-fast", messages=MESSAGES, extra_body={"bogus": 1})
    assert "Unrecognized request argument supplied: bogus" in exc.value.message


def test_rate_limit():
    client = azure_client(key=RL_KEY)
    try:
        with pytest.raises(openai.RateLimitError) as exc:
            for _ in range(2):
                client.chat.completions.create(model="wwz-rl", messages=MESSAGES)
    finally:
        client.close()
    assert exc.value.status_code == 429
    assert exc.value.code == "429"
    # Deployments Standard mandam retry-after-ms (spec 006, 16.6).
    assert int(exc.value.response.headers["retry-after-ms"]) > 0


def test_models(az):
    ids = [m.id for m in az.models.list().data]
    for want in ("wwz-fast", "wwz-tools", "wwz-reason", "wwz-rl"):
        assert want in ids


def test_v1_chat_and_responses():
    client = v1_client()
    try:
        r = client.chat.completions.create(model="wwz-fast", messages=MESSAGES)
        assert r.model == "wwz-fast"
        check_prompt_filter(r)
        resp = client.responses.create(model="wwz-fast", input="Say something short.")
        assert resp.object == "response"
        assert resp.status == "completed"
        assert resp.output_text
        with client.responses.stream(model="wwz-fast", input="Say something short.") as s:
            deltas = "".join(e.delta for e in s if e.type == "response.output_text.delta")
            final = s.get_final_response()
        assert deltas == final.output_text
        assert [m.id for m in client.models.list().data]
    finally:
        client.close()
