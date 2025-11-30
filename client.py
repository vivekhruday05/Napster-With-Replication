import argparse
import http.server
import socketserver
import threading
import time
import json
import os
import socket
import sys
import requests
from urllib.parse import urlparse, quote

# --- Configuration & Globals ---
SHARED_DIR = "shared"
PEER_ID = ""
PEER_ADDR = ""
BIND_PORT = 9000
SERVERS = []

class PeerRequestHandler(http.server.SimpleHTTPRequestHandler):
    """
    Handles file download requests from other peers.
    Maps /files/<filename> to the local SHARED_DIR.
    """
    def do_GET(self):
        # We only support /files/<name>
        if self.path.startswith("/files/"):
            filename = self.path[len("/files/"):]
            # Security: Prevent directory traversal
            if ".." in filename or filename.startswith("/"):
                self.send_error(400, "Bad Request")
                return
            
            file_path = os.path.join(SHARED_DIR, filename)
            if os.path.exists(file_path) and os.path.isfile(file_path):
                self.send_response(200)
                self.send_header("Content-type", "application/octet-stream")
                self.send_header("Content-Length", str(os.path.getsize(file_path)))
                self.end_headers()
                with open(file_path, 'rb') as f:
                    self.wfile.write(f.read())
            else:
                self.send_error(404, "File Not Found")
        else:
            self.send_error(404, "Not Found")

    def log_message(self, format, *args):
        # Silence server logs to keep CLI clean
        return

def get_local_ip():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        # Doesn't actually connect, just determines route
        s.connect(('8.8.8.8', 80))
        ip = s.getsockname()[0]
    except Exception:
        ip = '127.0.0.1'
    finally:
        s.close()
    return ip

def scan_files():
    """Scans SHARED_DIR and returns a list of file info dicts."""
    files = []
    if not os.path.exists(SHARED_DIR):
        os.makedirs(SHARED_DIR)
        
    for entry in os.listdir(SHARED_DIR):
        path = os.path.join(SHARED_DIR, entry)
        if os.path.isfile(path):
            stat = os.stat(path)
            # Basic info. In a real app, you'd read a metadata file for 'version'
            files.append({
                "name": entry,
                "size": stat.st_size,
                "hash": "",
                "version": int(stat.st_mtime * 1000) # Use mtime as version proxy
            })
    return files

def do_request(method, endpoint, payload=None):
    """
    Helper to send requests to the Primary Server (with failover logic if needed).
    """
    headers = {'Content-Type': 'application/json'}
    
    for base_url in SERVERS:
        url = base_url.rstrip('/') + endpoint
        try:
            if method == "POST":
                resp = requests.post(url, json=payload, headers=headers, timeout=5)
            else:
                resp = requests.get(url, params=payload, timeout=5)
            
            if resp.status_code >= 500:
                print(f"[debug] Server {base_url} error {resp.status_code}, trying next...")
                continue # Try next server
            
            return resp
        except requests.RequestException as e:
            # print(f"[debug] Connection to {base_url} failed: {e}")
            continue

    raise Exception("All servers failed")

# --- Core Client Logic ---

def register():
    files = scan_files()
    payload = {
        "peer": {"id": PEER_ID, "addr": PEER_ADDR},
        "files": files
    }
    try:
        resp = do_request("POST", "/register", payload)
        if resp.status_code == 200:
            data = resp.json()
            # print(f"[client] Registered {len(files)} files.")
            process_tasks(data.get("tasks", []))
        else:
            print(f"[error] Register failed: {resp.text}")
    except Exception as e:
        print(f"[error] Register error: {e}")

def heartbeat_loop():
    """Runs in background: Sends heartbeat every 20s"""
    while True:
        time.sleep(20)
        try:
            payload = {"peerId": PEER_ID}
            resp = do_request("POST", "/heartbeat", payload)
            if resp.status_code == 200:
                data = resp.json()
                if not data.get("ok"):
                    # Server doesn't know us, re-register
                    register()
                else:
                    process_tasks(data.get("tasks", []))
            else:
                # If server returns error, try registering
                register()
        except Exception as e:
            print(f"[heartbeat] Failed: {e}")

def process_tasks(tasks):
    """Handles replication tasks (auto-downloading files)"""
    if not tasks:
        return
    for task in tasks:
        file_info = task['file']
        source_peer = task['sourcePeer']
        print(f"\n[replication] Pulling {file_info['name']} from {source_peer['addr']}...")
        
        try:
            download_file_from_peer(source_peer['addr'], file_info['name'])
            # Announce that we now have it
            announce(file_info)
        except Exception as e:
            print(f"[replication] Failed to pull {file_info['name']}: {e}")

def announce(file_info):
    payload = {
        "peer": {"id": PEER_ID, "addr": PEER_ADDR},
        "file": file_info
    }
    do_request("POST", "/announce", payload)

def download_file_from_peer(peer_addr, filename):
    url = f"{peer_addr}/files/{quote(filename)}"
    resp = requests.get(url, stream=True, timeout=10)
    if resp.status_code != 200:
        raise Exception(f"Peer returned {resp.status_code}")
    
    dest_path = os.path.join(SHARED_DIR, filename)
    with open(dest_path, 'wb') as f:
        for chunk in resp.iter_content(chunk_size=8192):
            f.write(chunk)
    print(f"[client] Downloaded: {filename}")

# --- CLI Commands ---

def cmd_search(query):
    try:
        # Search is GET /search?q=...
        resp = do_request("GET", f"/search?q={quote(query)}")
        if resp.status_code == 200:
            matches = resp.json().get("matches", [])
            if not matches:
                print("No matches found.")
            for m in matches:
                fname = m['file']['name']
                ver = m['file']['version']
                hosts = [p['addr'] for p in m['peers']]
                print(f"Found: {fname} (v{ver}) on {', '.join(hosts)}")
        else:
            print(f"Search error: {resp.text}")
    except Exception as e:
        print(f"Search failed: {e}")

def cmd_get(filename):
    try:
        # 1. Find peers: GET /peers?file=...
        resp = do_request("GET", f"/peers?file={quote(filename)}")
        if resp.status_code != 200:
            print(f"File not found on network.")
            return

        data = resp.json() # Expect SearchMatch {file:..., peers:[...]}
        peers = data.get("peers", [])
        if not peers:
            print("No active peers have this file.")
            return

        # 2. Pick first peer
        target = peers[0]
        print(f"Downloading from {target['addr']}...")
        
        download_file_from_peer(target['addr'], filename)
        
        # 3. Announce ownership
        announce(data['file'])
        
    except Exception as e:
        print(f"Get failed: {e}")

def cmd_update(filename):
    # 1. Acquire Lease
    print(f"Requesting lease for {filename}...")
    try:
        lease_resp = do_request("POST", "/lease", {
            "peerId": PEER_ID,
            "fileName": filename
        })
        if lease_resp.status_code != 200:
            print(f"Lease request failed: {lease_resp.text}")
            return
            
        lease_data = lease_resp.json()
        if not lease_data.get("granted"):
            print(f"Lease DENIED: {lease_data.get('message')}")
            return
            
        print(f"Lease acquired! Valid until {lease_data.get('expiration')}")
        
        # 2. Simulate "Edit" (Update local timestamp/content)
        fpath = os.path.join(SHARED_DIR, filename)
        if not os.path.exists(fpath):
            print("File does not exist locally to update.")
            return
            
        # Update mtime to simulate edit
        os.utime(fpath, None)
        new_version = int(time.time() * 1000)
        
        # 3. Announce Update
        updated_info = {
            "name": filename,
            "size": os.path.getsize(fpath),
            "hash": "",
            "version": new_version
        }
        announce(updated_info)
        print(f"Update announced with version {new_version}")

    except Exception as e:
        print(f"Update failed: {e}")

def cmd_list():
    files = scan_files()
    print(f"Local files in '{SHARED_DIR}':")
    for f in files:
        print(f" - {f['name']} ({f['size']} bytes)")

# --- Main Entry Point ---

def run_peer_server():
    handler = PeerRequestHandler
    # Allow address reuse to avoid "Address already in use" errors on restart
    socketserver.TCPServer.allow_reuse_address = True
    # Parse port from BIND_PORT (int or string)
    port = int(BIND_PORT)
    try:
        httpd = socketserver.ThreadingTCPServer(("", port), handler)
        # print(f"Peer server listening on port {port}...")
        httpd.serve_forever()
    except OSError as e:
        print(f"[fatal] Could not bind to port {port}: {e}")
        os._exit(1)

def main():
    global SERVERS, SHARED_DIR, BIND_PORT, PEER_ADDR, PEER_ID

    parser = argparse.ArgumentParser(description="Napster Python Client")
    parser.add_argument("--server", default="http://localhost:8080", help="Comma-separated server addresses")
    parser.add_argument("--dir", default="shared", help="Folder to share files from")
    parser.add_argument("--bind", default="9000", help="Port to bind peer server")
    parser.add_argument("--addr", default="", help="Public address (e.g. http://1.2.3.4:9000)")
    args = parser.parse_args()

    SERVERS = args.server.split(",")
    SHARED_DIR = args.dir
    BIND_PORT = args.bind
    
    # Ensure shared directory exists
    if not os.path.exists(SHARED_DIR):
        os.makedirs(SHARED_DIR)

    # Determine Peer Address and ID
    if not args.addr:
        local_ip = get_local_ip()
        PEER_ADDR = f"http://{local_ip}:{BIND_PORT}"
    else:
        PEER_ADDR = args.addr

    # Generate ID from Addr (e.g., http://1.2.3.4:9000 -> peer-1.2.3.4-9000)
    parsed = urlparse(PEER_ADDR)
    PEER_ID = "peer-" + parsed.netloc.replace(":", "-")

    print(f"--- Python Napster Client ---")
    print(f"ID: {PEER_ID}")
    print(f"Dir: {SHARED_DIR}")
    print(f"Addr: {PEER_ADDR}")
    print(f"Servers: {SERVERS}")
    print("-----------------------------")

    # 1. Start Peer Server in background thread
    t_server = threading.Thread(target=run_peer_server, daemon=True)
    t_server.start()

    # 2. Initial Register
    register()

    # 3. Start Heartbeat in background thread
    t_hb = threading.Thread(target=heartbeat_loop, daemon=True)
    t_hb.start()

    # 4. Interactive CLI Loop
    print("\nCommands: search <q>, get <file>, update <file>, list, exit")
    try:
        while True:
            line = input("> ").strip()
            if not line:
                continue
            
            parts = line.split()
            cmd = parts[0].lower()
            args = parts[1:]

            if cmd == "search":
                if not args: print("Usage: search <query>"); continue
                cmd_search(" ".join(args))
            elif cmd == "get":
                if not args: print("Usage: get <filename>"); continue
                cmd_get(args[0])
            elif cmd == "update":
                if not args: print("Usage: update <filename>"); continue
                cmd_update(args[0])
            elif cmd == "list":
                cmd_list()
            elif cmd in ["exit", "quit"]:
                print("Exiting...")
                sys.exit(0)
            else:
                print("Unknown command. Try: search, get, update, list, exit")
    except KeyboardInterrupt:
        print("\nExiting...")
        sys.exit(0)

if __name__ == "__main__":
    main()