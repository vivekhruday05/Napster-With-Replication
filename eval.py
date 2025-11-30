#!/usr/bin/env python3
import requests
import time
import random
import string
import concurrent.futures
import statistics
import numpy as np
import matplotlib.pyplot as plt
import subprocess
import os
import sys

# --- Configuration ---
# Paths are relative to the script location
SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
BIN_SERVER = os.path.join(SCRIPT_DIR, "bin", "server")  # Expects compiled binary here
SERVER_PRIMARY = "http://localhost:8080"
SERVER_SHADOW = "http://localhost:8081"
SERVERS = [SERVER_PRIMARY, SERVER_SHADOW]

# Benchmark Settings
NUM_PEERS = 50
NUM_FILES_PER_PEER = 5
SEARCH_REQUESTS = 100
CONCURRENCY = 10
PEER_TTL = 45  # seconds (fixed in server.go)
SERVER_GRACE_PERIOD = 16 # server.go has 15s initGracePeriod; we wait 16s to be safe

# Endpoints that are considered "writes" and MUST go to the Primary only.
WRITE_ENDPOINTS = ["/announce", "/register", "/lease", "/delete", "/heartbeat"]

# --- Helper Functions ---

def random_string(length=8):
    return ''.join(random.choices(string.ascii_letters + string.digits, k=length))

def generate_peer_payload(peer_id, files=None, stable_addr=None):
    if files is None:
        files = []
        for _ in range(NUM_FILES_PER_PEER):
            files.append({
                "name": f"file_{random_string(5)}.txt",
                "size": random.randint(1000, 50000),
                "hash": random_string(16),
                "version": 1
            })
    if stable_addr is None:
        addr = f"http://192.168.1.{random.randint(1,255)}:9000"
    else:
        addr = stable_addr

    payload = {
        "peer": {
            "id": peer_id,
            "addr": addr,
            "lastSeen": "0001-01-01T00:00:00Z"
        },
        "files": files
    }
    return payload

class RobustClient:
    """Simulates a client with failover logic but ensures writes only go to Primary."""
    def __init__(self):
        self.session = requests.Session()

    def reset_session(self):
        try:
            self.session.close()
        except Exception:
            pass
        self.session = requests.Session()

    def request(self, method, endpoint, **kwargs):
        """
        Tries servers in order until one succeeds or all fail.
        Special rule: POST writes (announce/register/lease) go to Primary only.
        """
        # ensure endpoints start with '/'
        if not endpoint.startswith("/"):
            endpoint = "/" + endpoint

        # If this is a write endpoint, direct to primary only.
        if method.upper() == "POST" and any(endpoint.startswith(e) for e in WRITE_ENDPOINTS):
            url = f"{SERVER_PRIMARY}{endpoint}"
            try:
                if method.upper() == "GET":
                    resp = self.session.get(url, timeout=5, **kwargs)
                else:
                    resp = self.session.post(url, timeout=5, **kwargs)
                # For writes, propagate response (including 4xx) back to caller.
                return resp
            except requests.exceptions.RequestException as e:
                # Primary unavailable -> raise so tests can decide what to do (failover should be explicit)
                raise ConnectionError(f"Primary ({SERVER_PRIMARY}) error for {endpoint}: {e}")

        # For reads (GET) and other safe calls, try primary then shadow (failover)
        errors = []
        for base_url in SERVERS:
            url = f"{base_url}{endpoint}"
            try:
                if method.upper() == "GET":
                    resp = self.session.get(url, timeout=5, **kwargs)
                else:
                    resp = self.session.post(url, timeout=5, **kwargs)

                # Treat 5xx as retryable (try next server if available)
                if resp.status_code >= 500:
                    errors.append(f"{base_url}: {resp.status_code}")
                    continue

                return resp  # return 200/4xx/403/404 etc.
            except requests.exceptions.RequestException as e:
                errors.append(f"{base_url}: {e}")
                continue

        # if we get here all servers failed
        raise ConnectionError(f"All servers failed for {endpoint}: {errors}")

# --- Process Manager ---

class ClusterManager:
    def __init__(self):
        self.primary = None
        self.shadow = None

    def start(self):
        print(f"[System] Starting Shadow Server (8081)...")
        env_shadow = os.environ.copy()
        env_shadow["NAPSTER_SERVER_ADDR"] = ":8081"
        # No NAPSTER_SHADOW_ADDR -> this becomes the shadow (read-only)
        self.shadow = subprocess.Popen([BIN_SERVER], env=env_shadow, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(1)

        print(f"[System] Starting Primary Server (8080)...")
        env_primary = os.environ.copy()
        env_primary["NAPSTER_SERVER_ADDR"] = ":8080"
        env_primary["NAPSTER_SHADOW_ADDR"] = "http://localhost:8081"
        env_primary["NAPSTER_REPLICATION_FACTOR"] = "2"
        self.primary = subprocess.Popen([BIN_SERVER], env=env_primary, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        
        print(f"[System] Waiting {SERVER_GRACE_PERIOD}s for Primary grace period...")
        time.sleep(SERVER_GRACE_PERIOD)
        print("[System] Cluster Ready.")

    def stop(self):
        print("\n[System] Shutting down servers...")
        if self.primary:
            try:
                self.primary.kill()
            except Exception:
                pass
        if self.shadow:
            try:
                self.shadow.kill()
            except Exception:
                pass

    def crash_primary(self):
        print("[System] CRASHING PRIMARY SERVER")
        if self.primary:
            try:
                self.primary.kill()
            except Exception:
                pass
            self.primary = None
            time.sleep(1)

    def restart_primary(self):
        print(f"[System] Restarting Primary Server (Wait {SERVER_GRACE_PERIOD}s)...")
        env_primary = os.environ.copy()
        env_primary["NAPSTER_SERVER_ADDR"] = ":8080"
        env_primary["NAPSTER_SHADOW_ADDR"] = "http://localhost:8081"
        env_primary["NAPSTER_REPLICATION_FACTOR"] = "2"
        self.primary = subprocess.Popen([BIN_SERVER], env=env_primary, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(SERVER_GRACE_PERIOD) 

# --- Test Cases ---

def test_replication_flow(client):
    """
    CORRECT REPLICATION TEST:
    Because the server returns replication tasks *immediately* inside the
    /register response for P2 (after grace period), we MUST read them there.
    """
    print("\n[Test] Replication Convergence...")

    # 1. Register P1 with file A
    file_a = {
        "name": "replication_test.txt",
        "size": 1234,
        "hash": "abc",
        "version": 1
    }
    p1_data = generate_peer_payload("peer_rep_1", [file_a])
    client.request("POST", "/register", json=p1_data)

    # 2. Register P2 — THIS RESPONSE CARRIES THE REPLICATION TASKS
    p2_data = generate_peer_payload("peer_rep_2", [])
    start_time = time.time()

    resp = client.request("POST", "/register", json=p2_data)
    body = resp.json()

    # ✔ CRITICAL: server.go returns replication tasks HERE
    tasks = body.get("tasks", [])

    # 3. If tasks were not returned (edge case), fallback to heartbeat
    if not tasks:
        print("  (Info) No tasks in register response, trying heartbeat...")
        for _ in range(5):
            time.sleep(1.0) # Wait a bit more for replication planner
            hb = client.request("POST", "/heartbeat", json={"peerId": "peer_rep_2"})
            hb_body = hb.json()
            if hb_body.get("tasks"):
                tasks = hb_body["tasks"]
                break

    if not tasks:
        print("  FAIL: P2 never received replication tasks (register+heartbeat).")
        return None

    # Take first task
    task_file = tasks[0]["file"]

    # 4. P2 announces completion (simulate download)
    announce_payload = {
        "peer": p2_data["peer"],
        "file": task_file
    }
    client.request("POST", "/announce", json=announce_payload)

    convergence_time = (time.time() - start_time) * 1000

    # 5. Validate using /search
    resp = client.request("GET", "/search", params={"q": "replication_test"})
    matches = resp.json().get("matches", [])

    if not matches:
        print("  FAIL: Search returned no results.")
        return None

    hosts = [p["id"] for p in matches[0]["peers"]]

    if "peer_rep_1" in hosts and "peer_rep_2" in hosts:
        print(f"  PASS: File replicated to {hosts}. Time: {convergence_time:.2f} ms")
        return convergence_time

    print(f"  FAIL: Expected P1 & P2, got {hosts}")
    return None

def test_versioning_consistency(client):
    """
    1. P1 updates file version (needs lease).
    2. P2 tries to update without lease (should fail).
    """
    print("\n[Test] Versioning & Consistency...")
    filename = "ver_test.txt"
    p1_id = "peer_ver_1"
    p2_id = "peer_ver_2"

    # Setup: Both have v1 and are registered
    f_v1 = {"name": filename, "size": 100, "version": 1}
    client.request("POST", "/register", json=generate_peer_payload(p1_id, [f_v1], stable_addr="http://127.0.0.1:9011"))
    client.request("POST", "/register", json=generate_peer_payload(p2_id, [], stable_addr="http://127.0.0.1:9012"))

    # 1. P1 acquires lease
    start = time.time()
    resp = client.request("POST", "/lease", json={"peerId": p1_id, "fileName": filename})
    try:
        granted = resp.json().get("granted")
    except Exception:
        print("  FAIL: Lease response was not JSON or missing fields.")
        return None

    if not granted:
        print("  FAIL: P1 could not get lease.")
        return None

    # P1 Announces v2
    f_v2 = f_v1.copy()
    f_v2["version"] = 2
    resp = client.request("POST", "/announce", json={"peer": {"id": p1_id, "addr": "x", "lastSeen": "2025-01-01T00:00:00Z"}, "file": f_v2})
    if resp.status_code != 204:
        print(f"  FAIL: P1 announce v2 rejected: {resp.status_code} - {resp.text}")
        return None
    write_latency = (time.time() - start) * 1000
    print(f"  PASS: P1 updated to v2. Write Latency: {write_latency:.2f} ms")

    # 2. P2 tries to Announce v3 (No Lease)
    f_v3 = f_v1.copy()
    f_v3["version"] = 3
    resp = client.request("POST", "/announce", json={"peer": {"id": p2_id, "addr": "y", "lastSeen": "2025-01-01T00:00:00Z"}, "file": f_v3})

    if resp.status_code == 403:
        print("  PASS: P2 prevented from unauthorized write (403 Forbidden).")
    else:
        print(f"  FAIL: P2 should have been blocked, got {resp.status_code}")

    return write_latency

def test_client_failure(client):
    """
    1. Register P_Fail.
    2. Verify P_Fail in search.
    3. Stop heartbeating.
    4. Wait for TTL (45s).
    5. Verify P_Fail is gone.
    """
    print("\n[Test] Client Failure (Pruning)...")
    print(f"  Waiting {PEER_TTL + 5}s for TTL expiry (this will take a minute)...")

    pid = "peer_fail_1"
    fname = "fail_test.txt"
    client.request("POST", "/register", json=generate_peer_payload(pid, [{"name": fname, "size": 1, "version": 1}], stable_addr="http://127.0.0.1:9021"))

    # Verify presence
    resp = client.request("GET", "/search", params={"q": fname})
    matches = resp.json().get("matches", [])
    if not matches or pid not in [p["id"] for p in matches[0]["peers"]]:
        print("  FAIL: Setup failed, peer not found.")
        return

    # Wait
    time.sleep(PEER_TTL + 5)

    # Verify absence
    resp = client.request("GET", "/search", params={"q": fname})
    matches = resp.json().get("matches", [])
    if not matches:
        print("  PASS: Peer pruned successfully.")
    else:
        hosts = [p["id"] for p in matches[0]["peers"]]
        if pid in hosts:
            print(f"  FAIL: Peer {pid} still exists after TTL.")
        else:
            print("  PASS: Peer pruned successfully.")

def test_server_failover(cluster, client):
    """
    1. Establish baseline.
    2. Crash Primary.
    3. Measure recovery time (requests failing over to Shadow).
    4. Restart Primary.
    """
    print("\n[Test] Server Failover...")

    # Baseline
    try:
        client.request("GET", "/healthz")
    except Exception:
        print("  FAIL: Baseline health check failed.")
        return None

    cluster.crash_primary()

    start = time.time()
    success = False
    attempts = 0

    # Try to connect until success (reads will failover to shadow)
    while time.time() - start < 10:
        try:
            client.request("GET", "/healthz")
            success = True
            break
        except Exception:
            attempts += 1
            time.sleep(0.1)

    duration = (time.time() - start) * 1000
    if success:
        print(f"  PASS: Recovered in {duration:.2f} ms (switched to Shadow).")
        cluster.restart_primary()
        return duration
    else:
        print("  FAIL: Did not recover within 10s.")
        cluster.restart_primary()
        return None

def test_shadow_read_only():
    print("\n[Test] Shadow Read-Only Policy...")
    s = requests.Session()
    shadow = SERVER_SHADOW

    def try_post(ep, payload):
        try:
            resp = s.post(shadow + ep, json=payload, timeout=3)
            return resp.status_code
        except Exception as e:
            print(f"  WARN: {ep} error: {e}")
            return None

    reg_payload = generate_peer_payload("shadow_test_peer", [])
    ann_payload = {"peer": {"id": "shadow_test_peer", "addr": "http://127.0.0.1:9999", "lastSeen": "0001-01-01T00:00:00Z"}, "file": {"name": "x.txt", "size": 1, "version": 1}}
    lease_payload = {"peerId": "shadow_test_peer", "fileName": "x.txt"}
    hb_payload = {"peerId": "shadow_test_peer"}

    statuses = {
        "/register": try_post("/register", reg_payload),
        "/announce": try_post("/announce", ann_payload),
        "/lease": try_post("/lease", lease_payload),
        "/heartbeat": try_post("/heartbeat", hb_payload),
    }

    ok = True
    for ep, code in statuses.items():
        if code != 403:
            print(f"  FAIL: {ep} on shadow expected 403, got {code}")
            ok = False
        else:
            print(f"  PASS: {ep} rejected with 403")

    return ok

def test_failover_with_shadow_readonly(cluster, client):
    print("\n[Test] Failover with Shadow Read-Only...")

    # Step 1: Crash primary
    cluster.crash_primary()
    time.sleep(0.5)

    # Step 2: Writes to shadow should be rejected
    s = requests.Session()
    def post_shadow(ep, payload):
        try:
            return s.post(SERVER_SHADOW + ep, json=payload, timeout=3).status_code
        except Exception as e:
            print(f"  WARN(shadow): {ep} err: {e}")
            return None

    reg_payload = generate_peer_payload("failover_peer", [])
    ann_payload = {"peer": {"id": "failover_peer", "addr": "http://127.0.0.1:9998", "lastSeen": "0001-01-01T00:00:00Z"}, "file": {"name": "f.txt", "size": 1, "version": 1}}
    lease_payload = {"peerId": "failover_peer", "fileName": "f.txt"}
    hb_payload = {"peerId": "failover_peer"}

    shadow_codes = {
        "/register": post_shadow("/register", reg_payload),
        "/announce": post_shadow("/announce", ann_payload),
        "/lease": post_shadow("/lease", lease_payload),
        "/heartbeat": post_shadow("/heartbeat", hb_payload),
    }
    shadow_ok = all(code == 403 for code in shadow_codes.values())
    for ep, code in shadow_codes.items():
        if code == 403:
            print(f"  PASS: Shadow {ep} -> 403")
        else:
            print(f"  FAIL: Shadow {ep} expected 403, got {code}")

    # Step 3: Restart primary
    cluster.restart_primary()
    
    # Step 4: Writes to primary should succeed
    s = requests.Session()
    def post_primary(ep, payload):
        try:
            return s.post(SERVER_PRIMARY + ep, json=payload, timeout=5)
        except Exception as e:
            print(f"  ERR(primary): {ep} {e}")
            return None

    resp_reg = post_primary("/register", reg_payload)
    resp_lease = post_primary("/lease", lease_payload)
    resp_ann = post_primary("/announce", ann_payload)

    primary_ok = (
        resp_reg is not None and resp_reg.status_code == 200 and
        resp_lease is not None and resp_lease.status_code == 200 and
        resp_ann is not None and resp_ann.status_code in (200, 204)
    )

    if primary_ok:
        print("  PASS: Primary accepts writes after restart.")
    else:
        print(f"  FAIL: Primary response codes - Reg:{resp_reg.status_code if resp_reg else 'None'} Lease:{resp_lease.status_code if resp_lease else 'None'} Ann:{resp_ann.status_code if resp_ann else 'None'}")

    return shadow_ok and primary_ok

def run_load_test(client):
    print(f"\n[Load Test] Registering {NUM_PEERS} peers, {SEARCH_REQUESTS} searches...")

    latencies = []

    def reg(i):
        start = time.time()
        client.request("POST", "/register", json=generate_peer_payload(f"load_peer_{i}"))
        return (time.time() - start) * 1000

    with concurrent.futures.ThreadPoolExecutor(max_workers=CONCURRENCY) as ex:
        latencies = list(ex.map(reg, range(NUM_PEERS)))

    print(f"  Avg Register Latency: {statistics.mean(latencies):.2f} ms")

    search_lats = []

    def search(_):
        start = time.time()
        client.request("GET", "/search", params={"q": "file"})
        return (time.time() - start) * 1000

    with concurrent.futures.ThreadPoolExecutor(max_workers=CONCURRENCY) as ex:
        search_lats = list(ex.map(search, range(SEARCH_REQUESTS)))

    print(f"  Avg Search Latency: {statistics.mean(search_lats):.2f} ms")
    return latencies, search_lats

def run_scalability_test(client, peer_counts):
    """
    Scalability test for different numbers of peers.
    Measures:
    - Register latency distribution
    - Search latency distribution
    - Percentiles (p50, p90, p95, p99)
    - Throughput (ops/sec)
    - Error rate
    """

    print("\n[Scalability Test] Starting...")

    # results bucket
    results = {}

    for N in peer_counts:
        print(f"\n  [Test] {N} peers")

        register_times = []
        search_times = []
        errors = 0

        t_start = time.time()

        # --- Register N peers ---
        # Use simple ThreadPool to simulate concurrent load
        with concurrent.futures.ThreadPoolExecutor(max_workers=min(20, N)) as executor:
            future_to_peer = {
                executor.submit(client.request, "POST", "/register", json=generate_peer_payload(f"scale_peer_{N}_{i}")): i 
                for i in range(N)
            }
            for future in concurrent.futures.as_completed(future_to_peer):
                try:
                    # Measure rough request time (not precise client-side due to queueing, but indicative of system)
                    # Ideally we measure inside the task, but wrapper limits that. 
                    # For simplicity, we assume successful return is what matters.
                    future.result() 
                except Exception:
                    errors += 1
        
        # We simulate latency measurement by doing a subset sequentially to get clean numbers, 
        # or we accept that we didn't measure per-request latency in the block above for simplicity.
        # Let's run a separate measurement block for latency stats after bulk load.
        
        # Measure Latency (Sampled)
        measure_count = min(100, N)
        for i in range(measure_count):
            try:
                # Measure Register Latency
                t0 = time.time()
                client.request("POST", "/register", json=generate_peer_payload(f"scale_probe_{N}_{i}"))
                register_times.append((time.time() - t0) * 1000)
            except Exception:
                errors += 1

            try:
                # Measure Search Latency
                t0 = time.time()
                client.request("GET", "/search", params={"q": "file"})
                search_times.append((time.time() - t0) * 1000)
            except Exception:
                errors += 1

        t_end = time.time()
        
        # Approximate throughput: Total operations (Bulk load + Probe) / Total Time
        # Note: This includes the time to generate payloads and client-side overhead.
        total_ops = N + (measure_count * 2) 
        throughput = total_ops / (t_end - t_start)

        # --- Percentiles ---
        if register_times:
            reg_p = np.percentile(register_times, [50, 90, 95, 99])
        else:
            reg_p = [0, 0, 0, 0]

        if search_times:
            sea_p = np.percentile(search_times, [50, 90, 95, 99])
        else:
            sea_p = [0, 0, 0, 0]

        results[N] = {
            "register_p": reg_p,
            "search_p": sea_p,
            "throughput": throughput,
            "errors": errors,
            "error_rate": (errors / total_ops) * 100 if total_ops > 0 else 0,
            "reg_avg": np.mean(register_times) if register_times else 0,
            "search_avg": np.mean(search_times) if search_times else 0,
        }

        print(f"    Registers: avg={results[N]['reg_avg']:.2f}ms p99={reg_p[3]:.1f}")
        print(f"    Search:    avg={results[N]['search_avg']:.2f}ms p99={sea_p[3]:.1f}")
        print(f"    Throughput: {throughput:.2f} ops/sec  | Errors={errors}")

    return results

# --- Main ---

def main():
    if not os.path.exists(BIN_SERVER):
        print(f"Error: Server binary not found at {BIN_SERVER}")
        print("Please run: go build -o bin/server ./server")
        return

    cluster = ClusterManager()
    client = RobustClient()
    
    try:
        cluster.start()
        
        # -------------------------
        # Metrics storage
        # -------------------------
        metrics = {
            "replication_convergence": [],
            "write_latency": [],
            "failover_recovery": [],
        }

        # -------------------------
        # Core Tests
        # -------------------------
        print("\n========== CORE TESTS ==========")
        
        res_rep = test_replication_flow(client)
        if res_rep: metrics["replication_convergence"].append(res_rep)

        res_write = test_versioning_consistency(client)
        if res_write: metrics["write_latency"].append(res_write)
        
        res_failover = test_server_failover(cluster, client)
        if res_failover: metrics["failover_recovery"].append(res_failover)

        # Shadow read-only policy
        shadow_ok = test_shadow_read_only()

        # Failover flow with shadow read-only, then primary restore
        failover_ro_ok = test_failover_with_shadow_readonly(cluster, client)

        # -------------------------
        # Load Test
        # -------------------------
        print("\n========== LOAD TEST ==========")
        reg_lats, search_lats = run_load_test(client)

        # -------------------------
        # Scalability Test
        # -------------------------
        print("\n========== SCALABILITY TEST ==========")
        peer_scale_list = [10, 20, 50, 100, 200, 500, 1000] 
        scale_results = run_scalability_test(client, peer_scale_list)

        # -------------------------
        # Pruning Test
        # -------------------------
        print("\n========== CLIENT FAILURE (PRUNING) ==========")
        test_client_failure(client)

        # -------------------------
        # Plotting
        # -------------------------
        print("\n[Output] Generating plots...")

        # --- Figure 1: Core Benchmarks & Latency Dist ---
        plt.figure(figsize=(15, 6))
        
        # 1. Latency Histograms
        plt.subplot(1, 3, 1)
        plt.hist(reg_lats, bins=20, alpha=0.6, label='Register', color='#1f77b4')
        plt.hist(search_lats, bins=20, alpha=0.6, label='Search', color='#ff7f0e')
        plt.legend()
        plt.title("Request Latency (Load Test)")
        plt.xlabel("Latency (ms)")
        plt.ylabel("Frequency")

        # 2. System Metrics Bar Chart
        plt.subplot(1, 3, 2)
        m_names = ['Rep. Time', 'Write Lat.', 'Failover Rec.']
        m_values = [
            np.mean(metrics["replication_convergence"]) if metrics["replication_convergence"] else 0,
            np.mean(metrics["write_latency"]) if metrics["write_latency"] else 0,
            np.mean(metrics["failover_recovery"]) if metrics["failover_recovery"] else 0
        ]
        bars = plt.bar(m_names, m_values, color=['#2ca02c', '#9467bd', '#d62728'])
        plt.bar_label(bars, fmt='%.1f ms')
        plt.title("Core System Latencies")
        plt.ylabel("Time (ms)")

        # 3. Scalability Throughput (Simplified)
        plt.subplot(1, 3, 3)
        xs = list(scale_results.keys())
        ys = [scale_results[n]["throughput"] for n in xs]
        plt.plot(xs, ys, marker="o", color="purple", linewidth=2)
        plt.title("Throughput Scaling")
        plt.xlabel("Number of Peers")
        plt.ylabel("Ops / Sec")
        plt.grid(True, alpha=0.3)

        plt.tight_layout()
        plt.savefig("benchmark_extended.png")
        print("Saved benchmark_extended.png")

        # --- Figure 2: Detailed Scalability Metrics ---
        plt.figure(figsize=(16, 10))
        
        # Subplot 1: Throughput
        plt.subplot(2, 2, 1)
        plt.plot(xs, [scale_results[n]["throughput"] for n in xs], marker='o', color='purple')
        plt.title("System Throughput")
        plt.ylabel("Operations / Sec")
        plt.xlabel("Peer Count")
        plt.grid(True)

        # Subplot 2: Average Latency (Reg vs Search)
        plt.subplot(2, 2, 2)
        plt.plot(xs, [scale_results[n]["reg_avg"] for n in xs], marker='s', label="Register", color="blue")
        plt.plot(xs, [scale_results[n]["search_avg"] for n in xs], marker='^', label="Search", color="orange")
        plt.title("Average Request Latency")
        plt.ylabel("Latency (ms)")
        plt.xlabel("Peer Count")
        plt.legend()
        plt.grid(True)

        # Subplot 3: P99 Tail Latency
        plt.subplot(2, 2, 3)
        plt.plot(xs, [scale_results[n]["register_p"][3] for n in xs], marker='s', label="Register P99", color="blue", linestyle="--")
        plt.plot(xs, [scale_results[n]["search_p"][3] for n in xs], marker='^', label="Search P99", color="orange", linestyle="--")
        plt.title("Tail Latency (P99)")
        plt.ylabel("Latency (ms)")
        plt.xlabel("Peer Count")
        plt.legend()
        plt.grid(True)

        # Subplot 4: Error Rate
        plt.subplot(2, 2, 4)
        plt.plot(xs, [scale_results[n]["error_rate"] for n in xs], marker='x', color="red")
        plt.title("Error Rate")
        plt.ylabel("Error %")
        plt.xlabel("Peer Count")
        plt.ylim(bottom=-0.5) # Start at 0
        plt.grid(True)

        plt.suptitle("Scalability Stress Test Results", fontsize=16)
        plt.savefig("scalability_metrics.png")
        print("Saved scalability_metrics.png")

    except KeyboardInterrupt:
        print("\nAborted by user.")
    except Exception as e:
        print(f"\nAn error occurred: {e}")
        import traceback
        traceback.print_exc()
    finally:
        cluster.stop()

if __name__ == "__main__":
    main()