"""Fixtures dos testes de SDK Python (spec 018, "Testes de SDK").

Rodam contra um wwz já no ar com tests/sdk/wwz.yaml:

    go run ./cmd/wwz serve --config tests/sdk/wwz.yaml
    WWZ_SDK_BASE_URL=http://127.0.0.1:8000 WWZ_SDK_KEY=sk-wwz-sdk \
        tmp/venv/bin/python -m pytest -q tests/sdk/python

Sem WWZ_SDK_BASE_URL e WWZ_SDK_KEY todos os testes saem como "pulado".
"""

import os

import anthropic
import openai
import pytest

BASE_URL = os.environ.get("WWZ_SDK_BASE_URL", "").rstrip("/")
KEY = os.environ.get("WWZ_SDK_KEY", "")
# Chave com rpm: 1 restrita ao modelo wwz-rl (tests/sdk/wwz.yaml).
RL_KEY = os.environ.get("WWZ_SDK_RL_KEY", "sk-wwz-sdk-rl")
BAD_KEY = "sk-wwz-sdk-invalid-key"

# Mount de cada driver em tests/sdk/wwz.yaml, na forma de base_url do SDK.
OPENAI_COMPAT = {
    "openai": "/openai/v1",
    "openrouter": "/openrouter/api/v1",
    "opencode": "/opencode/zen/go/v1",
}
ANTHROPIC_MOUNT = "/anthropic"

TIMEOUT = 30.0


def pytest_collection_modifyitems(config, items):
    if BASE_URL and KEY:
        return
    skip = pytest.mark.skip(reason="WWZ_SDK_BASE_URL/WWZ_SDK_KEY ausentes")
    for item in items:
        item.add_marker(skip)


def openai_client(driver, key=None):
    return openai.OpenAI(
        base_url=BASE_URL + OPENAI_COMPAT[driver],
        api_key=key or KEY,
        max_retries=0,
        timeout=TIMEOUT,
        # O OpenCode exige a sessão, como o real (ADR 0059).
        default_headers={"x-opencode-session": "ses_wwz_sdk"} if driver == "opencode" else None,
    )


def anthropic_client(key=None):
    return anthropic.Anthropic(
        base_url=BASE_URL + ANTHROPIC_MOUNT,
        api_key=key or KEY,
        max_retries=0,
        timeout=TIMEOUT,
    )


@pytest.fixture(params=sorted(OPENAI_COMPAT))
def driver(request):
    return request.param


@pytest.fixture
def oai(driver):
    client = openai_client(driver)
    yield client
    client.close()


@pytest.fixture
def anth():
    client = anthropic_client()
    yield client
    client.close()


def retry_after_seconds(headers):
    """Lê retry-after (segundos inteiros ou fracionários) dos headers."""
    value = headers.get("retry-after")
    assert value is not None, f"429 sem retry-after: {dict(headers)}"
    seconds = float(value)
    assert seconds > 0, f"retry-after não positivo: {value!r}"
    return seconds
