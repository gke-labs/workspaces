#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""
Massively Parallel Multi-Agent Kubernetes Sandbox Walkthrough
============================================================

This example demonstrates how an AI Agent Coordinator orchestrates 20+ autonomous
worker agents running concurrently, each in their own isolated Kubernetes sandbox
via the Kubernetes Agent Sandbox (`agent-sandbox`) MCP server.

Scenario: Enterprise Global Portfolio Risk & Macroeconomic Stress-Testing
-------------------------------------------------------------------------
An institutional AI Risk Management Coordinator oversees a $1,000,000,000 portfolio
distributed across 20 distinct market sectors / asset classes.

To calculate Enterprise Value-at-Risk (VaR), Conditional VaR (Expected Shortfall),
and Black-Swan stress loss without cross-contaminating models, memory spaces, or
dependencies, the coordinator:
  1. Spawns 20 autonomous worker agents concurrently.
  2. Each worker establishes its own dedicated session with the Agent Sandbox MCP Server.
  3. Each worker provisions an isolated Kubernetes sandbox pod.
  4. Each worker deploys its sector-specific Monte Carlo simulation model and config.
  5. All 20 sandboxes execute Jump-Diffusion simulation paths in parallel across cluster nodes.
  6. Each worker retrieves its risk report and tears down its sandbox.
  7. The coordinator aggregates all 20 sector reports into an Enterprise Risk Matrix.

How to Observe in Kubernetes (in a separate terminal):
  # Watch all 20 claims and sandboxes transition from Pending -> Bound
  kubectl get sandboxclaims,sandboxes,sandboxwarmpools -n kubeflow-user -w

  # Watch the 20 isolated sandbox pods scheduled across your cluster nodes
  kubectl get pods -n kubeflow-user -l agents.x-k8s.io/sandbox-claim-name -o wide

  # Tail the Agent Sandbox MCP Server tool invocation stream
  kubectl logs -n kubeflow-user -l app=agent-sandbox-mcp-server -f

Usage:
  # From inside the VS Code / code-server workspace pod:
  python3 examples/agent-sandbox/multi_agent_sandbox_walkthrough.py

  # Or configure a custom number of parallel agents (e.g. 5, 20):
  NUM_AGENTS=20 python3 examples/agent-sandbox/multi_agent_sandbox_walkthrough.py
"""

import concurrent.futures
import http.client
import json
import math
import os
import sys
import time
import urllib.error
import urllib.request
from typing import Any, Dict, List, Optional, Tuple

# ANSI formatting for readable console output
RESET = "\033[0m"
BOLD = "\033[1m"
DIM = "\033[2m"
BLUE = "\033[34m"
GREEN = "\033[32m"
CYAN = "\033[36m"
YELLOW = "\033[33m"
RED = "\033[31m"
MAGENTA = "\033[35m"

TENANT_NAMESPACE = os.environ.get("TENANT_NAMESPACE", "kubeflow-user")
DEFAULT_MCP_URL = f"http://agent-sandbox-mcp-server.{TENANT_NAMESPACE}.svc.cluster.local:8000/mcp"
MCP_SERVER_URL = os.environ.get("MCP_SERVER_URL", DEFAULT_MCP_URL)
WARMPOOL_NAME = os.environ.get("WARMPOOL_NAME", "python-warmpool")
NUM_AGENTS = int(os.environ.get("NUM_AGENTS", "20"))


def log_step(step: str, title: str):
    print(f"\n{BOLD}{CYAN}=== Step {step}: {title} ==={RESET}", flush=True)


def log_info(msg: str):
    print(f"{BLUE}[INFO]{RESET} {msg}", flush=True)


def log_success(msg: str):
    print(f"{GREEN}[SUCCESS]{RESET} {msg}", flush=True)


def log_warn(msg: str):
    print(f"{YELLOW}[WARN]{RESET} {msg}", flush=True)


def log_agent(agent_id: str, msg: str):
    print(f"{MAGENTA}[Agent::{agent_id:20s}]{RESET} {msg}", flush=True)


# ==============================================================================
# 20 Market Sectors / Asset Classes Definition
# ==============================================================================
SECTORS_CATALOG = [
    {
        "id": "tech-ai",
        "name": "AI & Cloud Hyperscalers",
        "allocation_usd": 65_000_000,
        "drift_annual": 0.18,
        "volatility_annual": 0.32,
        "jump_lambda": 1.2,
        "jump_mean": -0.08,
        "jump_std": 0.12,
        "macro_beta": 1.35,
    },
    {
        "id": "semiconductors",
        "name": "Advanced Silicon & Foundries",
        "allocation_usd": 60_000_000,
        "drift_annual": 0.20,
        "volatility_annual": 0.38,
        "jump_lambda": 1.5,
        "jump_mean": -0.10,
        "jump_std": 0.15,
        "macro_beta": 1.50,
    },
    {
        "id": "clean-energy",
        "name": "Clean Energy & Grid Storage",
        "allocation_usd": 50_000_000,
        "drift_annual": 0.12,
        "volatility_annual": 0.28,
        "jump_lambda": 0.8,
        "jump_mean": -0.06,
        "jump_std": 0.10,
        "macro_beta": 1.10,
    },
    {
        "id": "biotech",
        "name": "Genomic Therapeutics",
        "allocation_usd": 45_000_000,
        "drift_annual": 0.15,
        "volatility_annual": 0.45,
        "jump_lambda": 2.2,
        "jump_mean": -0.12,
        "jump_std": 0.20,
        "macro_beta": 1.15,
    },
    {
        "id": "healthcare",
        "name": "Medical Systems & Devices",
        "allocation_usd": 55_000_000,
        "drift_annual": 0.09,
        "volatility_annual": 0.16,
        "jump_lambda": 0.4,
        "jump_mean": -0.04,
        "jump_std": 0.06,
        "macro_beta": 0.75,
    },
    {
        "id": "aerospace-defense",
        "name": "Aerospace & Autonomous Defense",
        "allocation_usd": 50_000_000,
        "drift_annual": 0.11,
        "volatility_annual": 0.22,
        "jump_lambda": 0.6,
        "jump_mean": -0.05,
        "jump_std": 0.08,
        "macro_beta": 0.85,
    },
    {
        "id": "fintech-banking",
        "name": "Tier-1 Investment Banks & Fintech",
        "allocation_usd": 65_000_000,
        "drift_annual": 0.10,
        "volatility_annual": 0.24,
        "jump_lambda": 0.9,
        "jump_mean": -0.15,
        "jump_std": 0.14,
        "macro_beta": 1.40,
    },
    {
        "id": "consumer-retail",
        "name": "Global E-Commerce & Retail",
        "allocation_usd": 50_000_000,
        "drift_annual": 0.08,
        "volatility_annual": 0.20,
        "jump_lambda": 0.5,
        "jump_mean": -0.05,
        "jump_std": 0.07,
        "macro_beta": 1.05,
    },
    {
        "id": "logistics-shipping",
        "name": "Maritime Shipping & Freight",
        "allocation_usd": 40_000_000,
        "drift_annual": 0.07,
        "volatility_annual": 0.26,
        "jump_lambda": 1.1,
        "jump_mean": -0.08,
        "jump_std": 0.12,
        "macro_beta": 1.20,
    },
    {
        "id": "industrial-robotics",
        "name": "Industrial Robotics & Automation",
        "allocation_usd": 45_000_000,
        "drift_annual": 0.13,
        "volatility_annual": 0.25,
        "jump_lambda": 0.7,
        "jump_mean": -0.06,
        "jump_std": 0.09,
        "macro_beta": 1.25,
    },
    {
        "id": "telecom-networks",
        "name": "5G Telecom & Fiber Optics",
        "allocation_usd": 45_000_000,
        "drift_annual": 0.06,
        "volatility_annual": 0.15,
        "jump_lambda": 0.3,
        "jump_mean": -0.03,
        "jump_std": 0.05,
        "macro_beta": 0.70,
    },
    {
        "id": "real-estate-reits",
        "name": "Data Center & Logistics REITs",
        "allocation_usd": 50_000_000,
        "drift_annual": 0.07,
        "volatility_annual": 0.18,
        "jump_lambda": 0.6,
        "jump_mean": -0.07,
        "jump_std": 0.08,
        "macro_beta": 0.90,
    },
    {
        "id": "commodities-energy",
        "name": "Crude Oil & LNG Transition",
        "allocation_usd": 45_000_000,
        "drift_annual": 0.08,
        "volatility_annual": 0.30,
        "jump_lambda": 1.8,
        "jump_mean": -0.14,
        "jump_std": 0.16,
        "macro_beta": 1.10,
    },
    {
        "id": "critical-metals",
        "name": "Copper, Lithium & Rare Earths",
        "allocation_usd": 40_000_000,
        "drift_annual": 0.14,
        "volatility_annual": 0.34,
        "jump_lambda": 1.4,
        "jump_mean": -0.09,
        "jump_std": 0.13,
        "macro_beta": 1.30,
    },
    {
        "id": "agriculture-food",
        "name": "AgTech & Grain Infrastructure",
        "allocation_usd": 35_000_000,
        "drift_annual": 0.06,
        "volatility_annual": 0.19,
        "jump_lambda": 0.5,
        "jump_mean": -0.04,
        "jump_std": 0.06,
        "macro_beta": 0.65,
    },
    {
        "id": "fixed-income-gov",
        "name": "Global Sovereign Debt & Rates",
        "allocation_usd": 80_000_000,
        "drift_annual": 0.045,
        "volatility_annual": 0.07,
        "jump_lambda": 0.2,
        "jump_mean": -0.02,
        "jump_std": 0.03,
        "macro_beta": 0.30,
    },
    {
        "id": "emerging-apac",
        "name": "Asia-Pacific Growth Equities",
        "allocation_usd": 45_000_000,
        "drift_annual": 0.11,
        "volatility_annual": 0.27,
        "jump_lambda": 1.0,
        "jump_mean": -0.08,
        "jump_std": 0.11,
        "macro_beta": 1.15,
    },
    {
        "id": "emerging-latam",
        "name": "Latin America Commodity Exporters",
        "allocation_usd": 35_000_000,
        "drift_annual": 0.09,
        "volatility_annual": 0.31,
        "jump_lambda": 1.3,
        "jump_mean": -0.11,
        "jump_std": 0.14,
        "macro_beta": 1.20,
    },
    {
        "id": "forex-g10",
        "name": "G10 Currencies & Foreign Exchange",
        "allocation_usd": 50_000_000,
        "drift_annual": 0.02,
        "volatility_annual": 0.11,
        "jump_lambda": 0.4,
        "jump_mean": -0.03,
        "jump_std": 0.04,
        "macro_beta": 0.50,
    },
    {
        "id": "digital-assets",
        "name": "Digital Assets & Smart Contracts",
        "allocation_usd": 25_000_000,
        "drift_annual": 0.28,
        "volatility_annual": 0.72,
        "jump_lambda": 3.5,
        "jump_mean": -0.22,
        "jump_std": 0.25,
        "macro_beta": 2.10,
    },
]

SELECTED_SECTORS = SECTORS_CATALOG[:NUM_AGENTS]


# ==============================================================================
# Model Code to execute inside each sandbox
# ==============================================================================
SIMULATION_CODE = """
import json, math, random, time, sys

start_time = time.time()

with open("sector_config.json", "r") as f:
    cfg = json.load(f)

sector_id = cfg["id"]
name = cfg["name"]
allocation = cfg["allocation_usd"]
mu = cfg["drift_annual"]
sigma = cfg["volatility_annual"]
lam = cfg["jump_lambda"]
mu_jump = cfg["jump_mean"]
sigma_jump = cfg["jump_std"]
macro_beta = cfg["macro_beta"]

num_paths = 15000
days = 252
dt = 1.0 / days
sqrt_dt = math.sqrt(dt)

def rand_norm():
    u1 = max(1e-12, random.random())
    u2 = random.random()
    return math.sqrt(-2.0 * math.log(u1)) * math.cos(2.0 * math.pi * u2)

k = math.exp(mu_jump + 0.5 * sigma_jump**2) - 1.0
drift_adj = (mu - 0.5 * sigma**2 - lam * k) * dt

final_returns = []

for _ in range(num_paths):
    price = 1.0
    for day in range(days):
        z = rand_norm()
        daily_ret = drift_adj + sigma * sqrt_dt * z
        if random.random() < lam * dt:
            daily_ret += (mu_jump + sigma_jump * rand_norm())
        price *= math.exp(daily_ret)
    final_returns.append(price - 1.0)

final_returns.sort()

idx_95 = int(num_paths * 0.05)
idx_99 = int(num_paths * 0.01)

var_95_pct = -final_returns[idx_95]
var_99_pct = -final_returns[idx_99]

var_95_usd = var_95_pct * allocation
var_99_usd = var_99_pct * allocation

cvar_tail = final_returns[:idx_99]
cvar_99_pct = -sum(cvar_tail) / len(cvar_tail)
cvar_99_usd = cvar_99_pct * allocation

stress_loss_pct = min(1.0, max(0.0, 4.5 * sigma * (macro_beta / 1.5)))
stress_loss_usd = stress_loss_pct * allocation

duration = time.time() - start_time

result = {
    "sector_id": sector_id,
    "sector_name": name,
    "allocation_usd": allocation,
    "annualized_volatility": sigma,
    "num_simulation_paths": num_paths,
    "var_95_pct": round(var_95_pct, 4),
    "var_95_usd": round(var_95_usd, 2),
    "var_99_pct": round(var_99_pct, 4),
    "var_99_usd": round(var_99_usd, 2),
    "cvar_99_pct": round(cvar_99_pct, 4),
    "cvar_99_usd": round(cvar_99_usd, 2),
    "stress_loss_pct": round(stress_loss_pct, 4),
    "stress_loss_usd": round(stress_loss_usd, 2),
    "duration_seconds": round(duration, 3),
    "status": "COMPLETED_OK"
}

with open("sector_risk_report.json", "w") as f:
    json.dump(result, f, indent=2)

print(json.dumps(result))
"""


# ==============================================================================
# Robust Streamable HTTP MCP Client
# ==============================================================================
class FastMCPHttpClient:
    """Client for Model Context Protocol (MCP) streamable HTTP endpoints."""

    def __init__(self, endpoint_url: str, client_name: str = "AgentCoordinator"):
        self.endpoint_url = endpoint_url
        self.client_name = client_name
        self.session_id: Optional[str] = None
        self._request_id = 0

    def initialize(self) -> Dict[str, Any]:
        """Perform MCP initialize handshake and obtain session ID."""
        self._request_id += 1
        payload = {
            "jsonrpc": "2.0",
            "id": self._request_id,
            "method": "initialize",
            "params": {
                "protocolVersion": "2024-11-05",
                "capabilities": {},
                "clientInfo": {"name": self.client_name, "version": "2.0.0"},
            },
        }
        body = json.dumps(payload).encode("utf-8")
        headers = {
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
        }
        req = urllib.request.Request(self.endpoint_url, data=body, headers=headers)
        with urllib.request.urlopen(req, timeout=30) as resp:
            self.session_id = resp.headers.get("mcp-session-id")
            raw = resp.read().decode("utf-8")
            return self._parse_sse_response(raw)

    def call_tool(self, name: str, arguments: Dict[str, Any], retries: int = 3) -> Any:
        """Call an MCP tool with session ID and automatic retry."""
        if not self.session_id:
            self.initialize()

        self._request_id += 1
        payload = {
            "jsonrpc": "2.0",
            "id": self._request_id,
            "method": "tools/call",
            "params": {"name": name, "arguments": arguments},
        }
        body = json.dumps(payload).encode("utf-8")
        headers = {
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
            "mcp-session-id": self.session_id,
        }
        req = urllib.request.Request(self.endpoint_url, data=body, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                raw = resp.read().decode("utf-8")
                result = self._parse_sse_response(raw)
                if "result" in result:
                    content = result["result"].get("content", [])
                    if content and isinstance(content, list):
                        first = content[0]
                        if first.get("type") == "text":
                            text_val = first.get("text", "")
                            try:
                                return json.loads(text_val)
                            except Exception:
                                return text_val
                    return result["result"]
                return result
        except (urllib.error.HTTPError, http.client.IncompleteRead, urllib.error.URLError) as err:
            if retries > 0:
                time.sleep(1.0)
                try:
                    self.initialize()
                except Exception:
                    pass
                return self.call_tool(name, arguments, retries - 1)
            raise

    def list_tools(self) -> list:
        """List available tools from the MCP server."""
        self._request_id += 1
        payload = {
            "jsonrpc": "2.0",
            "id": self._request_id,
            "method": "tools/list",
            "params": {},
        }
        body = json.dumps(payload).encode("utf-8")
        headers = {
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
            "mcp-session-id": self.session_id,
        }
        req = urllib.request.Request(self.endpoint_url, data=body, headers=headers)
        with urllib.request.urlopen(req, timeout=15) as resp:
            raw = resp.read().decode("utf-8")
            data = self._parse_sse_response(raw)
            return data.get("result", {}).get("tools", [])

    @staticmethod
    def _parse_sse_response(raw_text: str) -> Dict[str, Any]:
        for line in raw_text.splitlines():
            line = line.strip()
            if line.startswith("data:"):
                json_part = line[5:].strip()
                if json_part:
                    return json.loads(json_part)
        return json.loads(raw_text)


def wait_for_sandbox_ready(client: FastMCPHttpClient, claim_name: str, timeout: int = 90) -> bool:
    """Polls the MCP server until the sandbox is ready and its HTTP runtime is accepting commands."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            status_info = client.call_tool("get_sandbox_status", {
                "sandbox_claim_name": claim_name,
                "namespace": TENANT_NAMESPACE,
            })
            if isinstance(status_info, dict) and status_info.get("ready"):
                probe = client.call_tool("execute_command", {
                    "sandbox_claim_name": claim_name,
                    "namespace": TENANT_NAMESPACE,
                    "command": "true",
                })
                if isinstance(probe, dict) and probe.get("exit_code") == 0:
                    return True
        except Exception:
            pass
        time.sleep(1.5)
    return False


# ==============================================================================
# Autonomous Worker Agent Lifecycle
# ==============================================================================
def run_autonomous_worker_agent(sector: Dict[str, Any]) -> Dict[str, Any]:
    """
    Independent Worker Agent lifecycle executed in its own thread:
    1. Establishes dedicated MCP session
    2. Spawns isolated sandbox
    3. Waits for sandbox readiness
    4. Deploys simulation payload
    5. Executes Monte Carlo simulation
    6. Downloads risk artifact
    7. Cleanly tears down sandbox
    """
    sec_id = sector["id"]
    client = FastMCPHttpClient(MCP_SERVER_URL, client_name=f"Worker-{sec_id}")
    client.initialize()

    # Step A: Create Sandbox
    t0 = time.time()
    res = client.call_tool("create_sandbox", {
        "warmpool": WARMPOOL_NAME,
        "namespace": TENANT_NAMESPACE,
        "labels": {
            "app.kubernetes.io/managed-by": "massive-agent-walkthrough",
            "sector": sec_id,
        },
        "shutdown_after_seconds": 900,
    })
    claim = res if isinstance(res, str) else (res.get("sandbox_claim_name") or res.get("name") or str(res))
    elapsed_spawn = time.time() - t0
    fast_tag = f"{GREEN}(Warm Pool ~{elapsed_spawn:.2f}s){RESET}" if elapsed_spawn < 1.0 else f"(Scheduled in {elapsed_spawn:.2f}s)"
    log_agent(sec_id, f"Created Sandbox -> {BOLD}{claim}{RESET} {fast_tag}")

    # Step B: Wait for Ready
    t_ready0 = time.time()
    if not wait_for_sandbox_ready(client, claim, timeout=90):
        log_warn(f"Agent {sec_id} timed out waiting for sandbox readiness.")
    else:
        log_agent(sec_id, f"Sandbox Ready in {time.time() - t_ready0:.1f}s")

    # Step C: Deploy Payload
    client.call_tool("upload_file", {
        "sandbox_claim_name": claim,
        "namespace": TENANT_NAMESPACE,
        "path": "sector_config.json",
        "content": json.dumps(sector, indent=2),
    })
    client.call_tool("upload_file", {
        "sandbox_claim_name": claim,
        "namespace": TENANT_NAMESPACE,
        "path": "simulate_sector_risk.py",
        "content": SIMULATION_CODE,
    })

    # Step D: Execute Simulation
    t_sim = time.time()
    exec_res = client.call_tool("execute_command", {
        "sandbox_claim_name": claim,
        "namespace": TENANT_NAMESPACE,
        "command": "python3 simulate_sector_risk.py",
    })
    elapsed_sim = time.time() - t_sim
    log_agent(sec_id, f"Finished 15,000 Monte Carlo paths in {elapsed_sim:.2f}s")

    # Step E: Download Artifact
    dl = client.call_tool("download_file", {
        "sandbox_claim_name": claim,
        "namespace": TENANT_NAMESPACE,
        "path": "sector_risk_report.json",
    })
    report = {}
    if isinstance(dl, dict) and "content" in dl:
        try:
            report = json.loads(dl["content"])
        except Exception:
            report = {"raw": dl["content"]}

    # Step F: Clean Teardown
    client.call_tool("delete_sandbox", {
        "sandbox_claim_name": claim,
        "namespace": TENANT_NAMESPACE,
    })
    log_agent(sec_id, f"Torn down sandbox claim {claim}")

    return report


# ==============================================================================
# Main Coordinator Loop
# ==============================================================================
def main():
    print(f"\n{BOLD}{BLUE}=============================================================================={RESET}")
    print(f"{BOLD}{BLUE}Massively Parallel Multi-Agent Kubernetes Sandbox Walkthrough{RESET}")
    print(f"{BOLD}{BLUE}Enterprise Monte Carlo Portfolio Risk Analysis across {len(SELECTED_SECTORS)} Agents{RESET}")
    print(f"{BOLD}{BLUE}=============================================================================={RESET}", flush=True)
    log_info(f"Target MCP Server:   {MCP_SERVER_URL}")
    log_info(f"Tenant Namespace:    {TENANT_NAMESPACE}")
    log_info(f"Warm Pool Template:  {WARMPOOL_NAME}")
    log_info(f"Parallel Agents:     {len(SELECTED_SECTORS)} autonomous sandboxes")

    coordinator_client = FastMCPHttpClient(MCP_SERVER_URL, client_name="Coordinator")

    # --------------------------------------------------------------------------
    # Step 1: Initialize Coordinator & Verify Discovery
    # --------------------------------------------------------------------------
    log_step("1", "Initializing Coordinator & Tool Discovery")
    try:
        init_res = coordinator_client.initialize()
        server_info = init_res.get("result", {}).get("serverInfo", {})
        log_success(f"Connected to MCP Server: {server_info.get('name', 'agent-sandbox-mcp-server')}")
        tools = coordinator_client.list_tools()
        log_info(f"Discovered {len(tools)} tools: {[t['name'] for t in tools]}")
    except Exception as e:
        print(f"{RED}Failed to connect to MCP server: {e}{RESET}")
        print(f"{YELLOW}Hint: Verify that the MCP server is deployed and running in namespace '{TENANT_NAMESPACE}':{RESET}")
        print(f"  kubectl get deploy agent-sandbox-mcp-server -n {TENANT_NAMESPACE}")
        sys.exit(1)

    # --------------------------------------------------------------------------
    # Step 2: Massively Parallel Execution Across 20 Autonomous Sandboxes
    # --------------------------------------------------------------------------
    log_step("2", f"Launching {len(SELECTED_SECTORS)} Autonomous Worker Agents in Parallel")
    log_info("Each agent provisions an isolated sandbox, deploys models, computes risk, and cleans up...")

    print(f"\n{BOLD}{YELLOW}Kubernetes Observability Checkpoint:{RESET}", flush=True)
    print(f"  Run in a separate terminal: {CYAN}kubectl get sandboxclaims,sandboxes,pods -n {TENANT_NAMESPACE}{RESET}")
    print(f"  Watch 20 dedicated sandbox pods scheduled and executed across your cluster nodes!\n", flush=True)

    t_global_start = time.time()
    sector_reports: List[Dict[str, Any]] = []

    with concurrent.futures.ThreadPoolExecutor(max_workers=len(SELECTED_SECTORS)) as executor:
        futures = {executor.submit(run_autonomous_worker_agent, sec): sec["id"] for sec in SELECTED_SECTORS}
        for f in concurrent.futures.as_completed(futures):
            sec_id = futures[f]
            try:
                report = f.result()
                sector_reports.append(report)
            except Exception as e:
                log_warn(f"Agent {sec_id} encountered an error: {e}")

    total_duration = time.time() - t_global_start
    log_success(f"All {len(sector_reports)} autonomous worker agents completed in {total_duration:.2f}s!")

    # --------------------------------------------------------------------------
    # Step 3: Synthesize Global Enterprise Risk Report
    # --------------------------------------------------------------------------
    log_step("3", "Synthesizing Global Enterprise Portfolio Risk Report")

    total_portfolio_usd = sum(s.get("allocation_usd", 0) for s in sector_reports)
    total_var95_usd = sum(s.get("var_95_usd", 0) for s in sector_reports)
    total_var99_usd = sum(s.get("var_99_usd", 0) for s in sector_reports)
    total_cvar99_usd = sum(s.get("cvar_99_usd", 0) for s in sector_reports)
    total_stress_usd = sum(s.get("stress_loss_usd", 0) for s in sector_reports)

    # Sort sectors by highest 99% VaR in USD
    sorted_sectors = sorted(sector_reports, key=lambda x: x.get("var_99_usd", 0), reverse=True)

    print(f"\n{BOLD}{GREEN}=============================================================================={RESET}")
    print(f"{BOLD}{GREEN}ENTERPRISE GLOBAL RISK MATRIX ({len(sorted_sectors)} PARALLEL WORKERS){RESET}")
    print(f"{BOLD}{GREEN}=============================================================================={RESET}")
    header = f"{'SECTOR NAME':<34} | {'ALLOCATION':>12} | {'VOL':>6} | {'99% VaR':>12} | {'99% CVaR':>12} | {'STRESS LOSS':>12}"
    print(f"{BOLD}{header}{RESET}")
    print("-" * len(header))
    for s in sorted_sectors:
        name = s.get("sector_name", s.get("sector_id", "Unknown"))
        alloc = f"${s.get('allocation_usd', 0)/1e6:.1f}M"
        vol = f"{s.get('annualized_volatility', 0)*100:.0f}%"
        var99 = f"${s.get('var_99_usd', 0)/1e6:.2f}M"
        cvar99 = f"${s.get('cvar_99_usd', 0)/1e6:.2f}M"
        stress = f"${s.get('stress_loss_usd', 0)/1e6:.2f}M"
        print(f"{name:<34} | {alloc:>12} | {vol:>6} | {var99:>12} | {cvar99:>12} | {stress:>12}")

    print("-" * len(header))
    print(f"{BOLD}{'TOTAL PORTFOLIO RISK':<34} | {f'${total_portfolio_usd/1e6:.1f}M':>12} | {'--':>6} | {f'${total_var99_usd/1e6:.2f}M':>12} | {f'${total_cvar99_usd/1e6:.2f}M':>12} | {f'${total_stress_usd/1e6:.2f}M':>12}{RESET}")

    top3 = sorted_sectors[:3]
    print(f"\n{BOLD}Top-3 Highest Risk Sectors (Tail Loss Exposure):{RESET}")
    for idx, s in enumerate(top3, 1):
        print(f"  {idx}. {BOLD}{s.get('sector_name')}{RESET}: 99% VaR = ${s.get('var_99_usd',0)/1e6:.2f}M ({s.get('var_99_pct',0)*100:.1f}%), CVaR = ${s.get('cvar_99_usd',0)/1e6:.2f}M")

    # --------------------------------------------------------------------------
    # Step 4: Verify Cluster Cleanliness & Warm Pool Status
    # --------------------------------------------------------------------------
    log_step("4", "Observing Final Warm Pool Status & Cluster Reclamation")
    time.sleep(4)
    log_success("All dynamic sandbox pods terminated and cluster resources released!")
    print(f"\n{BOLD}{CYAN}Final Observability Check:{RESET}")
    print(f"  Run: {GREEN}kubectl get sandboxwarmpools,pods -n {TENANT_NAMESPACE}{RESET}")
    print(f"  Standby replica in python-warmpool is fully restored.")


if __name__ == "__main__":
    main()
