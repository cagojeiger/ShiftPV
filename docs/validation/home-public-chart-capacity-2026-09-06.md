# Home public chart capacity validation — 2026-09-06

## Scope

공개 Helm repository의 chart `0.1.4`, controller `0.1.5`, node `0.1.1`을 Home
MicroK8s에 임시 설치해 실제 kubelet 경로와 Pool 총예약 capacity admission을 검증했다.
대상은 Kubernetes `v1.35.6`의 Ready node 두 대였고 각 Pool은 기존 ext4 filesystem 아래
별도의 임시 directory를 사용했다. 기존 기본 StorageClass는 `microk8s-hostpath`로 유지했다.

## Kubelet root finding

chart 기본값 `/var/lib/kubelet`으로 설치했을 때 CSI workload는 기동했지만 application Pod는
mount에 실패했다. 이 cluster의 실제 kubelet state root는
`/var/snap/microk8s/common/var/lib/kubelet`이었다. 다음 override로 Helm upgrade하자 Node Plugin
DaemonSet이 rollout됐고 기존 Pending Pod를 다시 만들지 않아도 kubelet 재시도로 Ready에
수렴했다.

```bash
--set node.kubeletRootDir=/var/snap/microk8s/common/var/lib/kubelet
```

따라서 Node Plugin Pod의 Ready만으로 host path 정합성을 판정할 수 없다. 설치 시
`node.kubeletRootDir`와 실제 kubelet state root가 같아야 한다. 이 검증은 자동 감지를
증명하지 않으며 chart는 경로를 명시적으로 받는다.

## Capacity result

각 node에 `capacity.limit: 128Mi`인 `ShiftPVPool`을 등록했다. 첫 `64Mi` PVC는 Bound가 되고
Pod가 mount한 payload checksum이 host의 authoritative volume directory와 일치했다. 같은
node를 선택한 추가 `80Mi` PVC는 Pending을 유지했고 provisioning event는 다음 경계를
보고했다.

```text
ResourceExhausted: requested=83886080 reserved=67108864 limit=134217728
```

거부 중에도 첫 PVC, Pod, data는 Ready 상태와 checksum을 유지했다. 이는 Pool limit에서 현재
owner 기준 예약 합계를 뺀 입장 판단을 검증한다. PVC별 write quota나 외부 writer 통제는
검증 범위가 아니다.

## Cleanup

검증 PVC/PV/Volume을 정상 삭제한 뒤 두 Pool, Helm release, namespace, StorageClass,
CSIDriver, webhook과 Helm이 자동 제거하지 않는 세 ShiftPV CRD를 제거했다. 두 node의 빈 임시
directory도 제거했고 `microk8s-hostpath`는 계속 default였다. 로컬 Kind cluster도 남기지
않았다.
