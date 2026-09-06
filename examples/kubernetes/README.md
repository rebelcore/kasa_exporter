# Kubernetes Deployment

Device discovery is a UDP broadcast, and **a pod network cannot put a broadcast
on your LAN**. That is the only thing about this deployment that is unusual.

`hostNetwork: true` puts the pod on the node's own network so discovery works.
The node has to be on the same segment as your devices.

If it isn't — or your cluster will not allow host networking — drop
`hostNetwork` and `dnsPolicy` from `deployment.yaml` and name the devices
instead:

```yaml
          args:
            - "--no-kasa.discovery"
            - "--kasa.address=192.168.1.20,192.168.1.21"
```

## Installing

```bash
kubectl create namespace monitoring   # if you do not have one
kubectl apply -k .
kubectl -n monitoring logs -l app.kubernetes.io/name=kasa-exporter
```

Devices bound to a TP-Link account need a login; a fleet on the original
firmware needs none. Put real values in `secret.yaml`, or drop it from
`kustomization.yaml` if you need no credentials.

## Three things that will bite you

**Pod Security forbids `hostNetwork`.** A namespace enforcing `baseline` or
`restricted` rejects the pod. Either label the namespace
`pod-security.kubernetes.io/enforce=privileged` — which weakens it for
everything else in there — or use the address-list form above.

**Leave `strategy: Recreate` alone** while `hostNetwork` is on. The pod binds
port 9498 on the node, so a rolling update would start the new pod before the
old one let go and it would never bind.

**The ServiceMonitor's `release:` label must match your Prometheus**, or the
Operator ignores it and nothing appears on the Targets page:

```bash
kubectl get prometheus -A -o jsonpath='{..serviceMonitorSelector}'
```

## What is deliberately not here

These manifests are the minimum that works. The image already runs as `nobody`,
so there is no `securityContext` restating it, and a `seccompProfile` would be
pointless next to `hostNetwork` because the profile that requires one forbids
host networking anyway.

If your cluster enforces resource quotas, add requests — the exporter is small
and mostly idle:

```yaml
          resources:
            requests:
              cpu: 25m
              memory: 32Mi
            limits:
              memory: 128Mi
```
