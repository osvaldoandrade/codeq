#!/usr/bin/env python3
import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


BASE_URL = os.getenv("CODEQ_BASE_URL", "http://localhost:8080").rstrip("/")
PRODUCER_TOKEN = os.getenv("PRODUCER_TOKEN", "producer-token")
DEFAULT_KEY_PATH = os.path.expanduser("~/.codeq/local/worker.pem")
WORKER_KEY_PATH = os.getenv("CODEQ_WORKER_KEY_PATH", DEFAULT_KEY_PATH)


GO_JWT_GEN = r'''
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
	"crypto/x509"
)

func main() {
	outDir := os.Getenv("OUT_DIR")
	if outDir == "" {
		outDir = "."
	}
	kid := "codeq-local-1"
	iss := "codeq-test"
	aud := "codeq-worker"
	sub := "worker-1"

	keyPath := os.Getenv("KEY_PATH")
	var key *rsa.PrivateKey
	if keyPath != "" {
		if b, err := os.ReadFile(keyPath); err == nil {
			if block, _ := pem.Decode(b); block != nil {
				if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
					key = k
				} else if k2, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
					if rk, ok := k2.(*rsa.PrivateKey); ok {
						key = rk
					}
				}
			}
		}
	}
	if key == nil {
		var err error
		key, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		if keyPath != "" {
			_ = os.MkdirAll(filepath.Dir(keyPath), 0o755)
			pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
			_ = os.WriteFile(keyPath, pemBytes, 0o600)
		}
	}
	pub := key.PublicKey
	mod := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	exp := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	jwks := map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"kid": kid,
			"alg": "RS256",
			"use": "sig",
			"n":   mod,
			"e":   exp,
		}},
	}
	jwksBytes, err := json.Marshal(jwks)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "jwks.json"), jwksBytes, 0o644); err != nil {
		panic(err)
	}

	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	now := time.Now().Unix()
	payload := map[string]any{
		"iss":        iss,
		"aud":        aud,
		"sub":        sub,
		"exp":        now + 3600,
		"iat":        now - 10,
		"jti":        "jid-1",
		"eventTypes": []string{"GENERATE_MASTER"},
		"scope":      "codeq:claim codeq:heartbeat codeq:abandon codeq:nack codeq:result codeq:subscribe",
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	h := enc(header)
	p := enc(payload)
	signingInput := h + "." + p
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		panic(err)
	}
	s := base64.RawURLEncoding.EncodeToString(sig)
	jwt := signingInput + "." + s
	if err := os.WriteFile(filepath.Join(outDir, "worker.jwt"), []byte(jwt), 0o600); err != nil {
		panic(err)
	}
	fmt.Println(jwt)
}
'''


def run(cmd, env=None):
	result = subprocess.run(cmd, check=False, text=True, capture_output=True, env=env)
	if result.returncode != 0:
		raise RuntimeError(f"command failed: {' '.join(cmd)}\n{result.stderr.strip()}")
	return result.stdout.strip()


def http_request(method, path, token=None, body=None):
	url = f"{BASE_URL}{path}"
	headers = {}
	data = None
	if body is not None:
		data = json.dumps(body).encode("utf-8")
		headers["Content-Type"] = "application/json"
	if token:
		headers["Authorization"] = f"Bearer {token}"
	req = urllib.request.Request(url, data=data, headers=headers, method=method)
	try:
		with urllib.request.urlopen(req, timeout=10) as resp:
			return resp.status, resp.read().decode("utf-8")
	except urllib.error.HTTPError as e:
		return e.code, e.read().decode("utf-8")
	except urllib.error.URLError as e:
		raise RuntimeError(f"request failed: {method} {url}: {e}") from e


def wait_health(max_attempts=20, delay=1.0):
	for attempt in range(1, max_attempts + 1):
		try:
			status, body = http_request("GET", "/healthz")
			if status == 200:
				print(status, body)
				return True
		except RuntimeError:
			pass
		time.sleep(delay)
	return False


def main():
	print(f"[codeq] base_url={BASE_URL}")

	with tempfile.TemporaryDirectory(prefix="codeq-local-") as tmpdir:
		go_file = os.path.join(tmpdir, "codeq_jwt_gen.go")
		with open(go_file, "w", encoding="utf-8") as f:
			f.write(GO_JWT_GEN.strip() + "\n")

		env = dict(os.environ)
		env["OUT_DIR"] = tmpdir
		env["KEY_PATH"] = WORKER_KEY_PATH
		run(["go", "run", go_file], env=env)

		jwks_path = os.path.join(tmpdir, "jwks.json")
		worker_jwt_path = os.path.join(tmpdir, "worker.jwt")
		jwks = open(jwks_path, "r", encoding="utf-8").read()
		worker_token = open(worker_jwt_path, "r", encoding="utf-8").read().strip()

		patch = json.dumps([{
			"op": "replace",
			"path": "/spec/template/spec/containers/0/args",
			"value": ["-text", jwks],
		}])
		print("[codeq] updating jwks-mock")
		run(["kubectl", "patch", "deployment", "jwks-mock", "--type=json", "-p", patch])
		run(["kubectl", "rollout", "status", "deployment/jwks-mock"])

		print("[codeq] healthz")
		if not wait_health():
			raise RuntimeError("healthz failed; check port-forward")

		job_id = f"j-{int(time.time())}"
		print("[codeq] create task")
		status, body = http_request(
			"POST",
			"/v1/codeq/tasks",
			token=PRODUCER_TOKEN,
			body={"command": "GENERATE_MASTER", "payload": {"jobId": job_id}},
		)
		print(status, body)
		task_id = json.loads(body).get("id")
		if not task_id:
			raise RuntimeError("task create failed")

		print("[codeq] claim task")
		claimed_id = None
		for attempt in range(1, 6):
			status, body = http_request(
				"POST",
				"/v1/codeq/tasks/claim",
				token=worker_token,
				body={"commands": ["GENERATE_MASTER"], "leaseSeconds": 60},
			)
			print(status, body)
			if status == 204:
				print("no tasks available")
				return
			if status != 200:
				try:
					err_msg = json.loads(body).get("error", "")
				except json.JSONDecodeError:
					err_msg = body
				if err_msg in {"jwks fetch error", "invalid signature"} and attempt < 5:
					time.sleep(1)
					continue
				raise RuntimeError("claim failed")
			claimed = json.loads(body)
			claimed_id = claimed.get("id")
			if claimed_id:
				break
		if not claimed_id:
			raise RuntimeError("claim failed")

		print("[codeq] submit result")
		status, body = http_request(
			"POST",
			f"/v1/codeq/tasks/{claimed_id}/result",
			token=worker_token,
			body={"status": "COMPLETED", "result": {"ok": True, "jobId": job_id}},
		)
		print(status, body)

		print("[codeq] get result")
		status, body = http_request(
			"GET",
			f"/v1/codeq/tasks/{claimed_id}/result",
			token=PRODUCER_TOKEN,
		)
		print(status, body)

	print("[codeq] done")


if __name__ == "__main__":
	try:
		main()
	except Exception as e:
		print(f"[codeq] error: {e}")
		sys.exit(1)
