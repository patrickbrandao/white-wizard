"""SDK anthropic contra o modo estrito do cliente (spec 020, spec 018, ADR 0074).

No /anthropic-cc (client_verification: claude-code) só o Claude Code 2.1.308
passa: o SDK oficial não acrescenta ?beta=true, então messages.create e
beta.messages.create recebem 403 forged_request_error com reason=query
(a exceção de permissão do SDK). No /anthropic (sem a opção) a mesma chamada
dá 200.
"""

import anthropic
import pytest

from conftest import ANTHROPIC_MOUNT, BASE_URL, KEY, TIMEOUT

CC_MOUNT = "/anthropic-cc"
MESSAGES = [{"role": "user", "content": "Hello there, reply with ok"}]
PREFIX = (
    "Client verification failed: this request does not match Claude Code 2.1.308; "
    "the client is forging Claude Code requests (query: "
)


def client(mount, credential):
    # credential: x-api-key (api_key) ou Authorization: Bearer (auth_token),
    # como nas capturas da spec 020.
    kw = {"api_key": KEY} if credential == "api_key" else {"api_key": None, "auth_token": KEY}
    return anthropic.Anthropic(base_url=BASE_URL + mount, max_retries=0, timeout=TIMEOUT, **kw)


def create(c, beta):
    if beta:
        return c.beta.messages.create(
            model="wwz-fast", max_tokens=16, messages=MESSAGES, betas=["claude-code-20250219"]
        )
    return c.messages.create(model="wwz-fast", max_tokens=16, messages=MESSAGES)


@pytest.mark.parametrize("credential", ["api_key", "auth_token"])
@pytest.mark.parametrize("beta", [False, True], ids=["messages", "beta.messages"])
def test_strict_mount_refuses_the_sdk(beta, credential):
    with client(CC_MOUNT, credential) as c:
        with pytest.raises(anthropic.PermissionDeniedError) as exc:
            create(c, beta)
    err = exc.value
    assert err.status_code == 403
    assert err.body["type"] == "error"
    assert err.body["error"]["type"] == "forged_request_error"
    assert err.body["error"]["message"].startswith(PREFIX)
    assert err.body["request_id"].startswith("req_")
    h = err.response.headers
    assert h["x-wwz-client-verification"] == "failed; reason=query"
    assert h["x-should-retry"] == "false"
    assert h["request-id"] == err.body["request_id"]
    assert err.request_id == err.body["request_id"]


@pytest.mark.parametrize("beta", [False, True], ids=["messages", "beta.messages"])
def test_mount_without_verification_serves_the_sdk(beta):
    with client(ANTHROPIC_MOUNT, "api_key") as c:
        m = create(c, beta)
    assert m.type == "message"
    assert m.role == "assistant"
    assert "".join(b.text for b in m.content if b.type == "text")
