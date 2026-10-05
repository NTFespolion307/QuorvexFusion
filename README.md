# QuorvexFusion

A self-hosted compute cluster in Go: machines on your LAN or anywhere on the
internet join a shared pool of CPU, RAM, and GPUs, and jobs submitted to the
pool are scheduled onto them automatically.

- **One binary** (`cluster`) for the controller, the workers and the CLI.
- **Workers only dial out** to the controller (gRPC over mutual TLS), so they
  work behind NAT, inside containers and on rented GPU boxes.
- **No external services**: state lives in an embedded SQLite database.
- Linux only (amd64 and arm64).

> **Status:** under active development. Working today: joining nodes,
> hardware/metrics reporting, scheduling, shell jobs, array jobs, retries,
> timeouts, live logs, cgroup limits, the installer, LAN discovery, the
> web UI and file transfer. Coming next: Docker/GPU tasks, ephemeral node
> cleanup, release binaries.

## Quick start

### 1. Install the controller

On the machine that will coordinate the cluster:

```sh
git clone https://github.com/NTFespolion307/QuorvexFusion.git
cd QuorvexFusion
sudo ./install.sh controller
```

The installer builds the binary (downloading Go temporarily if needed), asks
a few questions (ports, admin password, whether this machine should also run
a worker), and starts a `cluster-controller` systemd service. It ends by
printing the web UI address and a **join code** such as
`7KQ2-MX4P-9TRA-BH3W-C8NE`.

### 2. Join workers

On every other machine:

```sh
git clone https://github.com/NTFespolion307/QuorvexFusion.git
cd QuorvexFusion
sudo ./install.sh worker
```

It finds the controller on your LAN and asks for the join code. That's all:
the code also verifies that the controller is really yours (it contains a
pin of the controller's certificate), so there's no fingerprint to compare.
Codes are case-insensitive and the dashes are optional.

If the controller is on another network (VPN, cloud, internet), give its
address; port 7443 is assumed:

```sh
sudo ./install.sh worker --controller 203.0.113.7 --code 7KQ2-MX4P-9TRA-BH3W-C8NE
```

Need another code? On the controller: `cluster token create`
(`--max-uses 1`, `--expires 1h`, or `--manual-approve` to make new nodes
wait for `cluster nodes approve <node>`).

### 3. Run jobs

The installer configures the CLI on the controller for root and for the
user who ran `sudo`:

```sh
cluster status                                    # pool size and usage
cluster nodes                                     # machines in the pool
cluster submit -f -- uname -a                     # run one command, stream its output
cluster submit --cpus 4 --memory 8G -- ./render.sh 12
cluster submit --wait --array 1-500 -- 'python3 sim.py --seed {i}'
cluster submit --retries 2 --timeout 2h --gpus 1 -- python3 train.py
cluster jobs                                      # recent jobs
cluster jobs show <job>                           # tasks, nodes, exit codes
cluster logs -f <job>.<index>                     # live output of one task
cluster cancel <job>
```

Each task runs with `/bin/sh -c` on whichever node has room, in its own
working directory, as the unprivileged `cluster` user, inside a cgroup
limiting it to the CPUs and memory it asked for. `{i}` is replaced by the
array index. Tasks see `CLUSTER_TASK_ID`, `CLUSTER_ARRAY_INDEX`,
`CLUSTER_ATTEMPT` and, for GPU tasks, `CUDA_VISIBLE_DEVICES` limited to their
own GPUs.

To use the CLI from another computer:
`cluster login --controller CONTROLLER_IP:8443` (asks for the admin password).

## Files: inputs, outputs and scripts

Remote machines don't share your disk, so jobs carry their files with them:

```sh
# Inputs: files or folders from this computer, placed in every task's working directory
cluster submit --input scene.blend --input textures/ --output 'frames/*' --array 1-250:10 -- \
  'blender -b scene.blend -o //frames/f_#### -s {i} -e $(( {i} + 9 )) -a'

# A script is uploaded and run; arguments after -- are passed to it
cluster submit --script train.py --input data/ --output 'model/**' --gpus 1 -- --epochs 10

# Collect the outputs (array jobs get one folder per task index)
cluster outputs <job>              # into ./<job>/
cluster outputs <job> --list
```

- `--input SRC[:DEST]` places a file or folder at `DEST` (default: its name).
  Each file is uploaded once (identical files are stored once), verified by
  SHA-256, and cached on every node that downloads it, so running the same
  job again downloads nothing. Inputs are read-only in the task; copy them if
  the task must modify them.
- `--output GLOB` collects matching files after each task (`*` within a
  folder, `**` across folders; a folder name collects everything in it).
  Outputs of the attempt that completed the task are kept, for finished and
  timed-out tasks.
- All transfers resume after interruptions. Progress shows in the CLI and on
  the task page.
- **Shared storage**: if nodes mount the same storage (NFS, a NAS), start
  their workers with `--shared-storage /mnt/shared` (or set
  `CLUSTER_SHARED_STORAGE` in `/etc/cluster/worker.env`) and use
  `--shared-input datasets/big:data` to link a path from it without any
  transfer. Such jobs only run on nodes with shared storage.
- Workers keep at most 20 GiB of cached inputs by default
  (`--cache-max 100G` to change).

## Web UI

Open `https://CONTROLLER_IP:8443` and sign in with the admin password.

- **Dashboard**: pool size and usage, nodes and tasks, CPU/GPU charts by location.
- **Nodes**: live usage per machine; approve, drain, revoke and label nodes.
  Click a node for its hardware and live charts (per-core CPU, memory, network, GPUs).
- **Jobs**: submit jobs, follow progress, read live task output, cancel.
- **Join tokens**: create join codes for new machines.
- **Settings**: admin password, API tokens, the CA certificate.

The browser warns about the certificate because it is signed by the cluster's
own CA. Accept it once, or remove the warning for good by importing the CA
certificate (Settings → Download CA certificate) into your browser or OS
trust store.

## Managing the installation

| Task | Command |
|---|---|
| Upgrade after `git pull` | `sudo ./install.sh upgrade` |
| Remove, keep data | `sudo ./install.sh uninstall` |
| Remove everything | `sudo ./install.sh uninstall --purge` |
| Controller logs | `journalctl -u cluster-controller -f` |
| Worker logs | `journalctl -u cluster-worker -f` |
| Worker settings (location, labels) | edit `/etc/cluster/worker.env`, then `sudo systemctl restart cluster-worker` |
| Change admin password | `sudo -u clusterctl cluster controller passwd --data-dir /var/lib/cluster` |
| See what a worker detects | `cluster worker probe` |

All installer options: `./install.sh controller --help`, `./install.sh worker --help`.

## Workers without systemd

Inside containers (or anywhere without systemd), run the worker in the
foreground under any process supervisor:

```sh
cluster worker --controller CONTROLLER_IP --code 7KQ2-MX4P-9TRA-BH3W-C8NE \
  --data-dir /var/lib/cluster-worker --location vastai --ephemeral
```

The code is only needed for the first run; the node's identity is saved in
`--data-dir`. Keep that directory on persistent storage if the container may
restart. Without root and systemd, CPU/memory limits are not enforced.

## Networking

Workers connect to the controller's node port (**7443**). Browsers and the
CLI use the web/API port (**8443**). Both use TLS with the controller's own
certificate authority; workers verify it by fingerprint, so they also work
when reaching the controller by IP, LAN name or VPN name.

- **On a LAN:** nothing else to do. Open the ports if a firewall is active
  (the installer prints the exact commands).
- **Over a VPN (Tailscale, Headscale, WireGuard):** install the controller
  with `--listen-ip <its VPN IP>` so it is only reachable over the VPN, and
  give workers that IP or VPN name.
- **Exposed to the internet:** forward/open 7443 (and 8443 if you want the
  UI remotely) and pass `--public-addr your.domain` so join commands use it.

More detail on each setup (and Let's Encrypt for the UI) is coming with the
remote-nodes milestone.

## Development

```sh
./dev.sh up 2                      # build and start a controller + 2 workers in ./dev-data
./dev.sh cli submit -f -- echo hi  # use the CLI against it
./dev.sh down                      # stop (./dev.sh clean also deletes the data)
make test                          # unit and integration tests
make cross                         # linux/amd64 and linux/arm64 binaries in dist/
```

The code is organised by responsibility under `internal/`:
`controller` (node sessions, task lifecycle), `scheduler` (pure placement
logic), `worker` and `runner` (agent and task execution), `store` (SQLite),
`api` (REST), `pki` (certificates), `hw` (hardware probes), `discovery` (mDNS).
The wire protocol is in `internal/clusterpb/cluster.proto`.
