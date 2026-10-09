#!/usr/bin/env python3
"""GPU smoke test and benchmark for a QuorvexFusion cluster.

Shows which GPU the task was given, checks that GPU results match the CPU,
and measures matrix-multiplication speed. Results are printed and saved to
results/gpu-benchmark.json (collect it with --output 'results/*').

Needs PyTorch: run it in a PyTorch image (--image pytorch/pytorch:...) or on
a node where `python3 -c "import torch"` works.
"""
import json
import os
import platform
import socket
import sys
import time

try:
    import torch
except ImportError:
    sys.exit("PyTorch is not installed on this node. Use a PyTorch image, e.g.\n"
             "  cluster submit --image pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime --gpus 1 ...")


def matmul_tflops(device, n, reps, dtype):
    """Times reps n x n matrix multiplications; returns TFLOPS."""
    a = torch.randn(n, n, device=device, dtype=dtype)
    b = torch.randn(n, n, device=device, dtype=dtype)
    torch.matmul(a, b)  # warm-up (kernel selection, memory allocation)
    if device == "cuda":
        torch.cuda.synchronize()
    start = time.perf_counter()
    for _ in range(reps):
        torch.matmul(a, b)
    if device == "cuda":
        torch.cuda.synchronize()
    seconds = time.perf_counter() - start
    return 2 * n ** 3 * reps / seconds / 1e12


def main():
    info = {
        "host": socket.gethostname(),
        "task": os.environ.get("CLUSTER_TASK_ID"),
        "cuda_visible_devices": os.environ.get("CUDA_VISIBLE_DEVICES", "(not set: container GPUs)"),
        "python": platform.python_version(),
        "torch": torch.__version__,
        "cuda_available": torch.cuda.is_available(),
    }
    print(f"Host {info['host']}, task {info['task']}, PyTorch {info['torch']}")
    print(f"CUDA_VISIBLE_DEVICES={info['cuda_visible_devices']}")

    if not torch.cuda.is_available():
        print("No GPU visible to this task. Was it submitted with --gpus 1? For containers, the node "
              "needs the NVIDIA container toolkit.")
        sys.exit(1)

    count = torch.cuda.device_count()
    props = torch.cuda.get_device_properties(0)
    info.update(gpu_count=count, gpu=props.name, gpu_memory_gib=round(props.total_memory / 2**30, 1),
                compute_capability=f"{props.major}.{props.minor}")
    print(f"GPUs visible: {count} -> using {props.name}, {info['gpu_memory_gib']} GiB, "
          f"compute capability {info['compute_capability']}")

    # Correctness: the GPU must agree with the CPU.
    torch.manual_seed(0)
    x = torch.randn(512, 512)
    y = torch.randn(512, 512)
    diff = (torch.matmul(x, y) - torch.matmul(x.cuda(), y.cuda()).cpu()).abs().max().item()
    info["max_difference_vs_cpu"] = diff
    print(f"GPU vs CPU max difference: {diff:.2e} ({'ok' if diff < 1e-2 else 'SUSPICIOUS'})")

    # Speed. Big enough to keep a modern GPU busy, small enough for 4 GB cards.
    n = 8192 if props.total_memory >= 6 * 2**30 else 4096
    info["gpu_tflops_fp32"] = round(matmul_tflops("cuda", n, 20, torch.float32), 2)
    print(f"GPU FP32 matmul ({n}x{n}): {info['gpu_tflops_fp32']} TFLOPS")
    if props.major >= 7:  # tensor cores
        info["gpu_tflops_fp16"] = round(matmul_tflops("cuda", n, 20, torch.float16), 2)
        print(f"GPU FP16 matmul ({n}x{n}): {info['gpu_tflops_fp16']} TFLOPS")

    threads = int(os.environ.get("OMP_NUM_THREADS", "1"))
    torch.set_num_threads(threads)
    info["cpu_threads"] = threads
    info["cpu_tflops_fp32"] = round(matmul_tflops("cpu", 2048, 3, torch.float32), 3)
    print(f"CPU FP32 matmul (2048x2048, {threads} thread(s)): {info['cpu_tflops_fp32']} TFLOPS")
    print(f"GPU is {info['gpu_tflops_fp32'] / max(info['cpu_tflops_fp32'], 1e-9):.0f}x faster than this task's CPU share")

    os.makedirs("results", exist_ok=True)
    with open("results/gpu-benchmark.json", "w") as f:
        json.dump(info, f, indent=2)
    print("Saved results/gpu-benchmark.json")


if __name__ == "__main__":
    main()
