#!/usr/bin/env python3
"""Run inside a real guest; wait for desktop approval and credential input."""
import json
import sys
import time
import urllib.request
import urllib.error

endpoint, upstream = sys.argv[1:]
sequence = 0
session = ''

def call(method, params, token='sk-bf-fixture-A'):
    global sequence, session
    sequence += 1
    headers = {'Content-Type': 'application/json', 'Accept': 'application/json, text/event-stream', 'Authorization': 'Bearer ' + token}
    if session:
        headers['Mcp-Session-Id'] = session
    request = urllib.request.Request(endpoint + '/mcp', data=json.dumps({'jsonrpc': '2.0', 'id': sequence, 'method': method, 'params': params}).encode(), headers=headers)
    with urllib.request.urlopen(request, timeout=30) as response:
        session = response.headers.get('Mcp-Session-Id', session)
        body = json.load(response)
    assert not body.get('error'), body
    return body['result']

call('initialize', {'protocolVersion': '2025-06-18', 'capabilities': {}, 'clientInfo': {'name': 'guest-desktop-fixture', 'version': '1'}})
call('tools/call', {'name': 'request_mcp_install', 'arguments': {'mcp_name': 'desktop', 'transport': 'http', 'endpoint': upstream, 'requested_tools': ['search'], 'credential_fields': ['api_key']}})
print('installation submitted from guest; waiting for frontend', flush=True)
deadline = time.monotonic() + 180
while True:
    tools = call('tools/list', {})['tools']
    if any(tool['name'] == 'desktop-search' for tool in tools):
        break
    assert time.monotonic() < deadline, 'installation not approved'
    time.sleep(0.5)
result = call('tools/call', {'name': 'desktop-search', 'arguments': {'api_key': '$PRIVACY_PROTECT_api_key', 'query': 'hello'}})
assert 'desktop-install-ok' in json.dumps(result), result
assert 'fixture-installed-secret' not in json.dumps(result)
session = ''
call('initialize', {'protocolVersion': '2025-06-18', 'capabilities': {}, 'clientInfo': {'name': 'other-scope', 'version': '1'}}, 'sk-bf-fixture-B')
assert not any(tool['name'] == 'desktop-search' for tool in call('tools/list', {}, 'sk-bf-fixture-B')['tools'])
for route in ['/api/governance/virtual-keys', '/v0/management/auth-files']:
    try:
        urllib.request.urlopen(endpoint + route, timeout=5)
        raise AssertionError('management route accessible')
    except urllib.error.HTTPError as error:
        assert error.code == 404, error
print('guest MCP invocation, credential substitution, scope isolation and management rejection: PASS', flush=True)
