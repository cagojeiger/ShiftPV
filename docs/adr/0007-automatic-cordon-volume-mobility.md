# 0007. 정상 cordon 이동은 Kubernetes 배치와 협력한다

- 상태: Accepted
- 날짜: 2026-09-02
- 후속 결정: 이동 전 중단 방지는 [ADR 0008](0008-nondisruptive-mobility-preflight.md)이 보완한다.
- 상세 계약: [volume mobility](../spec/volume-mobility.md)

## Context

한 node에 authoritative data가 있는 volume을 정비 중 옮기면서 PVC/PV identity와 workload
controller의 책임을 유지해야 한다. CSI에는 MoveVolume RPC가 없고 drain은 migration 완료를
기다리지 않는다.

## Decision

```mermaid
flowchart LR
    C[정상 source cordon] --> G[workload 대기]
    G --> P[placement Pod]
    P --> S[kube-scheduler 선택]
    S --> M[영속 journal 기반 cold migration]
```

ShiftPV는 배치 요청과 volume transaction을 조정한다. kube-scheduler가 destination을 선택하고,
workload controller가 replica를 소유한다. Commit 전 source, commit 후 destination이 유일한 owner다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| MutablePVNodeAffinity | alpha 기능과 장기 지원 경계에 의존한다. |
| custom scheduler/plugin | cluster-wide 구성과 결합도가 커진다. |
| workload replica/template 직접 변경 | GitOps와 workload controller의 소유권이 겹친다. |

## Consequences

자동 이동은 정상 source와 지원 가능한 단일-consumer workload에 적용한다. Cordon은 이동 신호이며,
운영자는 이동 완료를 확인한 뒤 drain을 진행한다.
