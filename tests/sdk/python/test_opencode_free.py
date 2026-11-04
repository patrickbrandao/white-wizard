"""SDK openai contra o modo public, os gratuitos e o Extra Usage do opencode.

Task 425 (ADR 0072, itens 4–7 e 12; spec 006 16.3). O User-Agent do
OpenCode (opencode/...) só vai ao wwz local: o teste se recusa a rodar se
WWZ_SDK_BASE_URL apontar para o opencode.ai.
"""

import datetime
import random

import openai
import pytest

from conftest import BASE_URL, TIMEOUT

PAYG = "/opencode/zen/v1"
GO = "/opencode/zen/go/v1"
OC_UA = "opencode/latest/2.2.5/wwz-sdk"
FREE = "wwz-free"
PAID = "wwz-fast"
EXTRA_KEY = "sk-wwz-oc-extra"
MESSAGES = [{"role": "user", "content": "Say something short."}]
FREE_TEXT = (
    "Free usage limit reached. Free models work in OpenCode (https://opencode.ai); "
    "subscribe to OpenCode Go to keep going: https://opencode.ai/go"
)


@pytest.fixture(autouse=True)
def never_the_real():
    # O header do OpenCode contorna um controle de acesso do real: só ao wwz.
    assert "opencode.ai" not in BASE_URL, BASE_URL


def client(base, key="public", ua=None):
    # Cada cliente tem um IP próprio (X-Forwarded-For, trusted_proxies de
    # loopback em tests/sdk/wwz.yaml): a cota diária dos gratuitos é por IP
    # sem chave, e a suíte pode rodar de novo no mesmo servidor.
    headers = {
        "x-opencode-session": "ses_wwz_sdk",
        "X-Forwarded-For": "2001:db8::%x:%x" % (random.getrandbits(16), random.getrandbits(16)),
    }
    if ua:
        headers["User-Agent"] = ua
    return openai.OpenAI(base_url=BASE_URL + base, api_key=key, max_retries=0, timeout=TIMEOUT, default_headers=headers)


def opencode_usage(r):
    return r.model_dump()["usage"]["opencode"]


def seconds_to_midnight():
    now = datetime.datetime.now(datetime.timezone.utc)
    midnight = (now + datetime.timedelta(days=1)).replace(hour=0, minute=0, second=0, microsecond=0)
    return (midnight - now).total_seconds()


def assert_free_usage_limit(err):
    assert err.status_code == 429
    assert err.response.json() == {"type": "error", "error": {"type": "FreeUsageLimitError", "message": FREE_TEXT}}
    ra = float(err.response.headers["retry-after"])
    assert abs(ra - seconds_to_midnight()) <= 3, ra


def test_public_free_with_opencode_client():
    with client(PAYG, ua=OC_UA) as c:
        r = c.chat.completions.create(model=FREE, messages=MESSAGES)
    assert r.choices[0].message.content
    u = opencode_usage(r)
    assert u["billing_source"] == "free", u
    assert r.usage.model_dump()["cost"] == 0


def test_public_free_generic_client():
    # Sem o User-Agent do OpenCode a cota é zero (conferido com curl,
    # free/public_generic_client.json).
    with client(PAYG) as c:
        with pytest.raises(openai.RateLimitError) as exc:
            c.chat.completions.create(model=FREE, messages=MESSAGES)
    assert_free_usage_limit(exc.value)


def test_public_paid_model():
    with client(PAYG, ua=OC_UA) as c:
        with pytest.raises(openai.AuthenticationError) as exc:
            c.chat.completions.create(model=PAID, messages=MESSAGES)
    assert exc.value.response.json() == {"type": "error", "error": {"type": "AuthError", "message": "Missing API key."}}


def test_public_go_base():
    with client(GO, ua=OC_UA) as c:
        with pytest.raises(openai.AuthenticationError) as exc:
            c.chat.completions.create(model=FREE, messages=MESSAGES)
    assert exc.value.response.json() == {
        "error": {"type": "server_error", "message": "Upstream request failed: Go requires a credential"}
    }


def test_free_daily_count():
    # free_requests_per_day: 2 (tests/sdk/wwz.yaml), contado no IP do cliente.
    with client(PAYG, ua=OC_UA) as c:
        for _ in range(2):
            assert opencode_usage(c.chat.completions.create(model=FREE, messages=MESSAGES))["billing_source"] == "free"
        with pytest.raises(openai.RateLimitError) as exc:
            c.chat.completions.create(model=FREE, messages=MESSAGES)
    assert_free_usage_limit(exc.value)


def test_go_quota_exhausted_extra_usage():
    # A 1ª requisição esgota a cota Go da conta; com Extra Usage a seguinte
    # é cobrada do saldo.
    with client(GO, key=EXTRA_KEY) as c:
        c.chat.completions.create(model=PAID, messages=MESSAGES)
        r = c.chat.completions.create(model=PAID, messages=MESSAGES)
    u = opencode_usage(r)
    assert u["billing_source"] == "balance", u
    assert r.usage.model_dump()["cost"] > 0
