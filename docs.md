### 1. The Liqo FAQ (The DaemonSet Problem)

The most direct explanation of this exact trap scenario is in the Liqo GitHub Repository FAQ. They explain why system pods (like Kube-Proxy) accidentally fall into this trap due to their massive tolerations.

* **Where to find it:** Search the Liqo documentation or their GitHub repo for the FAQ page.
* **Look for the question:** *"Why DaemonSets pods (e.g., Kube-Proxy, CNI pods) scheduled on virtual nodes are in OffloadingBackOff?"* * **What it explains:** It details how virtual nodes have a taint, and if a pod bypasses that taint without the namespace being offloaded, the virtual node actively rejects it.

### 2. The "Resource Reflection" Docs

This section explains the actual engine (the Virtual Kubelet Reflector) that threw the error in your terminal.

* **Where to find it:** `Docs -> Usage -> Resource Reflection`
* **What it explains:** It breaks down how the Liqo Virtual Kubelet translates local pods into remote `ShadowPods`. It notes that if a pod is manually forced to a virtual node in a non-offloaded namespace, the reflection engine refuses to translate it, leaving it stranded in a `Pending` state.

### 3. The "Namespace Offloading" Docs

This section explains the security mechanism you used to finally allow the pod through.

* **Where to find it:** `Docs -> Usage -> Namespace Offloading`
* **What it explains:** It details the `NamespaceOffloading` Custom Resource. It explains that by default, a namespace is isolated. Only when this CR is created does Liqo activate the mutating webhooks that dynamically inject the proper tolerations and routing labels into the pods, allowing the reflection engine to process them.