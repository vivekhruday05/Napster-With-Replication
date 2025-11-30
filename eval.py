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

# Endpoints that are considered "writes" and MUST go to the Primary only.
WRITE_ENDPOINTS = ["/announce", "/register", "/lease"]

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
        print("[System] Starting Shadow Server (8081)...")
        env_shadow = os.environ.copy()
        env_shadow["NAPSTER_SERVER_ADDR"] = ":8081"
        # No NAPSTER_SHADOW_ADDR -> this becomes the shadow
        self.shadow = subprocess.Popen([BIN_SERVER], env=env_shadow, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(1)

        print("[System] Starting Primary Server (8080)...")
        env_primary = os.environ.copy()
        env_primary["NAPSTER_SERVER_ADDR"] = ":8080"
        env_primary["NAPSTER_SHADOW_ADDR"] = "http://localhost:8081"
        env_primary["NAPSTER_REPLICATION_FACTOR"] = "2"
        self.primary = subprocess.Popen([BIN_SERVER], env=env_primary, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(2)  # Wait for startup

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
        print("[System] 💥 CRASHING PRIMARY SERVER 💥")
        if self.primary:
            try:
                self.primary.kill()
            except Exception:
                pass
            self.primary = None
            time.sleep(1)

    def restart_primary(self):
        print("[System] Restarting Primary Server...")
        env_primary = os.environ.copy()
        env_primary["NAPSTER_SERVER_ADDR"] = ":8080"
        env_primary["NAPSTER_SHADOW_ADDR"] = "http://localhost:8081"
        env_primary["NAPSTER_REPLICATION_FACTOR"] = "2"
        self.primary = subprocess.Popen([BIN_SERVER], env=env_primary, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(2)

def test_replication_flow(client):
    """
    CORRECT REPLICATION TEST:
    Because the server returns replication tasks *immediately* inside the
    /register response for P2, we MUST read them there, otherwise they are lost
    before heartbeat polling (queue is consumed).

    Steps:
    1. Register P1 with file A.
    2. Register P2 (empty) and CAPTURE the tasks from the register response.
    3. If empty (rare), fallback to heartbeat.
    4. Announce from P2.
    5. Search must list file on both peers.
    """
    print("\n[Test] Replication Convergence...")

    # 1. Register P1 with file
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
        for _ in range(5):
            time.sleep(0.3)
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

    # Setup: Both have v1 and are registered (ensure both peers known to server)
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
    resp = client.request("POST", "/announce", json={"peer": {"id": p1_id, "addr": "x"}, "file": f_v2})
    if resp.status_code != 204:
        print(f"  FAIL: P1 announce v2 rejected: {resp.status_code}")
        return None
    write_latency = (time.time() - start) * 1000
    print(f"  PASS: P1 updated to v2. Write Latency: {write_latency:.2f} ms")

    # 2. P2 tries to Announce v3 (No Lease)
    f_v3 = f_v1.copy()
    f_v3["version"] = 3
    resp = client.request("POST", "/announce", json={"peer": {"id": p2_id, "addr": "y"}, "file": f_v3})

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
    """
    Ensure Shadow server rejects client write operations with 403.
    Tests:
    - POST /register to shadow
    - POST /announce to shadow
    - POST /lease to shadow
    - POST /heartbeat to shadow
    """
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
    ann_payload = {"peer": {"id": "shadow_test_peer", "addr": "http://127.0.0.1:9999"}, "file": {"name": "x.txt", "size": 1, "version": 1}}
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
    """
    1) Crash Primary.
    2) Attempt writes against Shadow -> expect 403.
    3) Restart Primary.
    4) Attempt writes against Primary -> expect success (200/204).
    """
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
    ann_payload = {"peer": {"id": "failover_peer", "addr": "http://127.0.0.1:9998"}, "file": {"name": "f.txt", "size": 1, "version": 1}}
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
    time.sleep(1.0)

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
        print("  FAIL: Primary did not accept writes after restart.")

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
        for i in range(N):
            try:
                start = time.time()
                client.request("POST", "/register",
                    json=generate_peer_payload(f"scale_peer_{N}_{i}")
                )
                register_times.append((time.time() - start) * 1000)
            except Exception:
                errors += 1

        # --- Perform search queries ---
        # Query a common prefix to ensure lookup hits
        for _ in range(min(200, N)):
            try:
                start = time.time()
                client.request("GET", "/search", params={"q": "file"})
                search_times.append((time.time() - start) * 1000)
            except Exception:
                errors += 1

        t_end = time.time()
        total_ops = len(register_times) + len(search_times)
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
            "reg_avg": np.mean(register_times) if register_times else 0,
            "search_avg": np.mean(search_times) if search_times else 0,
        }

        print(f"    Registers: avg={results[N]['reg_avg']:.2f}ms p50={reg_p[0]:.1f} p90={reg_p[1]:.1f} p95={reg_p[2]:.1f} p99={reg_p[3]:.1f}")
        print(f"    Search:    avg={results[N]['search_avg']:.2f}ms p50={sea_p[0]:.1f} p90={sea_p[1]:.1f} p95={sea_p[2]:.1f} p99={sea_p[3]:.1f}")
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
        peer_scale_list = [10, 20, 50, 100, 200, 400, 600, 800, 1000]
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

        # --- Figure 1: Register/Search latency histograms
        plt.figure(figsize=(15, 5))
        plt.subplot(1, 3, 1)
        plt.hist(reg_lats, bins=15, alpha=0.5, label='Register', color='skyblue')
        plt.hist(search_lats, bins=15, alpha=0.5, label='Search', color='salmon')
        plt.legend()
        plt.title("Request Latency Distribution")
        plt.xlabel("ms")
        plt.ylabel("Count")

        # --- Figure 1 (middle): Core system metrics
        plt.subplot(1, 3, 2)
        m_names = ['Rep. Conv.', 'Write Lat.', 'Failover', 'Shadow RO']
        m_values = [
            np.mean(metrics["replication_convergence"]) if metrics["replication_convergence"] else 0,
            np.mean(metrics["write_latency"]) if metrics["write_latency"] else 0,
            np.mean(metrics["failover_recovery"]) if metrics["failover_recovery"] else 0,
            1 if shadow_ok else 0
        ]
        plt.bar(m_names, m_values, color=['green', 'orange', 'red', 'blue'])
        plt.title("System Performance Metrics")
        plt.ylabel("ms")

        # --- Figure 1 (right): Scalability Throughput Curve
        plt.subplot(1, 3, 3)
        xs = list(scale_results.keys())
        ys = [scale_results[n]["throughput"] for n in xs]
        plt.plot(xs, ys, marker="o", color="purple")
        plt.title("Scalability: Throughput vs Peer Count")
        plt.xlabel("Number of Peers")
        plt.ylabel("Throughput (ops/sec)")
        plt.grid(True)

        plt.tight_layout()
        plt.savefig("benchmark_extended.png")
        print("Saved benchmark_extended.png")

        # ---------------------
        # Save scalability plot separately
        # ---------------------
        plt.figure(figsize=(10, 5))
        plt.plot(xs, ys, marker='o')
        plt.title("Scalability Throughput Curve")
        plt.xlabel("Peer Count")
        plt.ylabel("Throughput (ops/sec)")
        plt.grid(True)
        plt.savefig("scalability_throughput.png")
        print("Saved scalability_throughput.png")

    except KeyboardInterrupt:
        print("\nAborted by user.")
    except Exception as e:
        print(f"\nAn error occurred: {e}")
    finally:
        cluster.stop()

if __name__ == "__main__":
    main()
