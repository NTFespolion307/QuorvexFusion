// Ready-made jobs offered on the "New job" form. Choosing one fills in the
// form; scripts are served by the controller from /ui/examples/ and attached
// as if picked from disk.

export const EXAMPLES = [
  {
    id: "hello",
    name: "Hello cluster: 8 small tasks spread over the nodes",
    fields: {
      command: 'echo "task {i} running on $(hostname) with $CLUSTER_CPUS CPU"; sleep 2',
      array: "8", cpus: "0.5",
    },
  },
  {
    id: "nvidia-smi",
    name: "GPU check: nvidia-smi on a GPU node",
    fields: { command: "nvidia-smi", gpus: "1" },
  },
  {
    id: "gpu-benchmark-container",
    name: "GPU benchmark in a PyTorch container (needs the NVIDIA container toolkit)",
    script: "gpu-benchmark.py",
    fields: { image: "pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime", gpus: "1", outputs: "results/*" },
  },
  {
    id: "gpu-benchmark-plain",
    name: "GPU benchmark with the node's own PyTorch",
    script: "gpu-benchmark.py",
    fields: { gpus: "1", outputs: "results/*" },
  },
  {
    id: "python-container",
    name: "Python in a container, writing an output file",
    fields: {
      image: "python:3.12-slim",
      command: 'mkdir -p out && python3 -c "import platform, os; print(platform.python_version(), os.cpu_count())" | tee out/info.txt',
      outputs: "out/*",
    },
  },
];

// The form fields an example may set; everything else is reset.
const FIELDS = ["command", "name", "image", "cpus", "memory", "gpus", "array", "retries", "timeout", "outputs"];
const DEFAULTS = { cpus: "1", gpus: "0", retries: "0" };

// applyExample fills the form from an example and attaches its script.
export async function applyExample(form, id) {
  const ex = EXAMPLES.find((e) => e.id === id);
  if (!ex) return;
  for (const f of FIELDS) form[f].value = ex.fields[f] ?? DEFAULTS[f] ?? "";
  const files = new DataTransfer();
  if (ex.script) {
    const res = await fetch("/ui/examples/" + ex.script);
    if (!res.ok) throw new Error(`could not load the example script (${res.status})`);
    files.items.add(new File([await res.blob()], ex.script, { type: "text/plain" }));
  }
  form.script.files = files.files;
  form.script.dispatchEvent(new Event("change"));
  form.array.dispatchEvent(new Event("input"));
}
