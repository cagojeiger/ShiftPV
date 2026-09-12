# ShiftPV Helm chart

Chart는 CSI runtime, StorageClass, admission endpoint, RBAC와 안전한 제거 lifecycle을 설치한다.
Node Pool은 storage 운영자가 별도로 등록한다.

```mermaid
flowchart TB
    H[Helm release] --> C[Controller Deployment]
    H --> N[Node Plugin DaemonSet]
    H --> SC[StorageClass]
    H --> CSI[CSIDriver + RBAC + Services]
    O[Storage operator] --> P[node별 ShiftPVPool]
    P --> C
    P --> N
```

## Install

```bash
helm repo add shiftpv https://cagojeiger.github.io/ShiftPV
helm repo update shiftpv
helm install shiftpv shiftpv/shiftpv \
  --namespace shiftpv-system --create-namespace \
  --wait
```

Repository 개발에서는 `shiftpv/shiftpv` 대신 `./charts/shiftpv`를 사용한다. Chart, Controller,
Node 버전은 독립적으로 release하고 공개 chart가 multi-architecture image tag를 선택한다.

### Kubelet state root

| Kubernetes 구성 | 값 |
|---|---|
| 일반 kubelet | `/var/lib/kubelet` |
| MicroK8s | `/var/snap/microk8s/common/var/lib/kubelet` |

MicroK8s 설치:

```bash
helm install shiftpv shiftpv/shiftpv \
  --namespace shiftpv-system --create-namespace \
  --set node.kubeletRootDir=/var/snap/microk8s/common/var/lib/kubelet \
  --wait
```

한 release는 kubelet root가 같은 node 집합을 담당한다. `node.nodeSelector`로 해당 집합을 선택한다.

## Metrics

```yaml
metrics:
  enabled: true
  port: 8080
  snapshotInterval: 30s
  prometheusRule:
    enabled: true
    additionalLabels:
      release: kube-prometheus-stack
  serviceMonitor:
    enabled: true
    interval: 30s
    scrapeTimeout: 5s
    additionalLabels:
      release: kube-prometheus-stack
```

| 설정 | 동작 |
|---|---|
| 기본값 | metrics, ServiceMonitor, PrometheusRule 모두 false |
| metrics 활성화 | Controller/Node 내부 HTTP endpoint와 ClusterIP Service 두 개 |
| ServiceMonitor 활성화 | 설치된 Prometheus Operator CRD 사용; `additionalLabels`를 Prometheus selector에 맞춤 |
| PrometheusRule 활성화 | 관측 실패·stale, Pool accounting/inventory, Cleanup/copy review 경보 |
| Node 관측 주기 | 기존 `poolReadiness.interval` 사용 |
| API 관측 주기 | `snapshotInterval`; timeout 10s |
| 접근 | monitoring namespace에서 metrics port로 접근 허용; 외부 공개와 별개 |

Endpoint는 인증 없는 내부 HTTP다. 네트워크 접근 범위는 운영 환경의 NetworkPolicy로 제한한다.
ServiceMonitor를 사용하지 않는 Prometheus도 두 Service의 endpoint를 발견해 수집할 수 있다.
Argo CD에서는 values를 Git으로 관리하고 Operator CRD를 먼저 설치한다.
[지표 계약](../../docs/spec/metrics.md)은 논리 예약과 filesystem 여유를 구분한다.

### Grafana dashboard

| 배포 구성 | 설정 |
|---|---|
| 같은 cluster의 Grafana sidecar | `metrics.dashboard.enabled: true`; 기본 label `grafana_dashboard: "1"` |
| Sidecar label 변경 | `metrics.dashboard.labels`를 Grafana selector에 맞춤 |
| 중앙 Grafana | Chart의 `dashboards/shiftpv-overview.json`을 중앙 cluster의 dashboard ConfigMap으로 GitOps 관리 |
| 수동 import | 동일 JSON을 import하고 Prometheus datasource 선택 |

Dashboard ConfigMap은 기본 false이며 Grafana를 설치하지 않는다. 활성화 시 release namespace에
생성된다. Grafana sidecar가 해당 namespace를 감시하도록 설정한다.
ServiceMonitor는 target에 `shiftpv="true"`를 추가한다. 직접 scrape하는 구성도 이 target label을
추가한다. Cluster·Namespace·Pool 변수를 제공하며 cluster label이 없는 단일 Prometheus에서는
Cluster를 All로 사용한다. `Driver namespace`는 ShiftPV 설치 namespace다.
Pool 선택은 `Pool comparison` 구역에만 적용되며 나머지 구역은 설치 전체를 보여준다.
`Observation window`는 화면의 관측 유효기간(기본 3분)이며 실제 Pool probe 주기보다 길게 선택한다.
이 설정은 storage admission이나 지표 수집 주기를 변경하지 않는다.

| Panel 묶음 | 운영 질문 |
|---|---|
| 상단 현재값 카드 | 발견된 target의 수집 상태, 관측 성공·freshness, 활성 Move, Blocked Volume |
| Pool 비교표 | Ready, 예약 집계 상태, filesystem 여유, 예약·한도·Volume CR 없는 예약 |
| 용량 추이 | filesystem 여유 비율, 예약 한도 사용 비율, inode 여유 |
| 이동 현재값 표 | 0보다 큰 Volume 상태·이동 보류 사유 |
| CSI 추이 | 완료 RPC 빈도·non-OK 응답·p95 처리 시간 |
| 관측 상세표 | source별 상태와 마지막 성공 이후 경과 시간 |

현재값의 수집 실패·유효기간 초과는 `Unknown`으로 표시하고 상태는 문자와 색상을 함께 사용한다.
상단 observation 상태는 오래된 관측을 `Attention`으로 표시한다. 추이에서는 유효하지 않은 구간이
끊어진다. 빈 이동 표는 해당 항목 0건 또는 관측 불가일 수 있으므로 상단 관측 상태를 함께 본다.
CSI 호출 전 빈 그래프는 `No data`다. 발견된 target 상태는 기대 target 전체가 존재한다는 보증과 구분한다.
Grafana는 관측 화면이며 PVC별 실제 사용량은 측정하지 않는다. 선택형 PrometheusRule은 같은 지표의
지속 장애만 알리고, 파일 사용량 quota나 application I/O 상태를 추론하지 않는다.

## Register Pools

각 참여 node에 기존 writable directory를 가리키는 immutable Pool 하나를 선언한다.

```yaml
apiVersion: shiftpv.io/v1alpha1
kind: ShiftPVPool
metadata:
  name: storage-worker-a
spec:
  nodeName: worker-a
  mountPath: /var/lib/shiftpv
  capacity:
    limit: 500Gi
```

```bash
kubectl wait --for=condition=Ready shiftpvpool/storage-worker-a --timeout=2m
```

| Pool 신호 | Ready 계약 |
|---|---|
| Path | 기존 absolute non-root directory |
| Access | directory lookup 성공 |
| Write | 임시 create, sync, cleanup 성공 |
| Capacity | filesystem `statfs` 성공 |
| Freshness | 마지막 probe가 `poolReadiness.staleAfter` 안에 있음 |
| Inventory | 최대 256 observations가 완전함; `truncated=false` |
| Ownership | node마다 Pool 하나, path는 immutable |
| Security | Pool CR 변경 권한은 storage operator에 한정 |

`capacity.limit`는 PVC reservation 총량을 제어한다. Filesystem available bytes는 같은 filesystem의
모든 writer를 포함한다. 별도 mount의 연속성은 OS monitoring이 담당하며 unmount 뒤 나타난 writable
directory는 ShiftPV 관점에서 일반 directory와 같은 입력이다.

Chart와 Controller는 Pool directory를 생성하지 않는다. Missing, non-directory, root(`/`), read-only,
permission failure, stale probe, truncated inventory는 신규 provisioning과 이동 후보에서 제외되며 기존 owner authority는
유지된다. `storageClass.defaultClass=true`는 새 PVC의 default 선택만 바꾸고 기존 hostPath PV는 유지한다.

## Configure

| Value | 역할 | 기본값 |
|---|---|---|
| `runtime.identityContract` | identity-aware helper와 node inventory 계약 활성화 | `false` |
| `controller.image.*` | Controller image | 공개 controller |
| `controller.replicas`, `controller.resources` | 단일 lifecycle writer와 Controller resource 정책 | `1`, `{}` |
| `node.image.*` | Node Plugin image | 공개 node |
| `node.kubeletRootDir` | kubelet state root | `/var/lib/kubelet` |
| `node.nodeSelector`, `node.tolerations` | 참여 node | empty |
| `node.resources` | Node Plugin resource 정책 | `{}` |
| `helperPod.image` | create/delete/GC helper image | `busybox:1.37` |
| `helperPod.timeout`, `helperPod.resources` | helper 실행 제한 | `2m`, 최소 requests/limits |
| `serviceAccount.helper.*` | identity helper 전용 최소권한 ServiceAccount | chart가 생성 |
| `poolReadiness.interval` | node-local probe 주기 | `1m` |
| `poolReadiness.staleAfter` | provisioning, mobility, GC의 공통 freshness window | `3m` |
| `mobility.enabled`, `mobility.webhookPort` | cordon mobility와 admission endpoint | `true`, `9443` |
| `mobility.interval` | event watch를 보완하는 safety interval | `30s` |
| `mobility.helperImage` | copy/promote helper image | 공개 controller |
| `lifecycle.uninstallMode` | Helm 또는 Argo CD 제거 owner | `helm` |
| `storageClass.create`, `storageClass.name` | StorageClass 생성과 이름 | `true`, `shiftpv` |
| `storageClass.defaultClass` | default-class annotation | `false` |
| `sidecars.*` | CSI sidecar image와 resource | pinned image |

`poolReadiness.staleAfter`는 한 번의 probe interval과 Kubernetes API 지연을 포함한다. 고정 driver
이름과 cluster-scoped resource에 따라 cluster마다 ShiftPV release 하나와 Controller replica 하나를 운영한다.

## Mobility and admission

```bash
kubectl label namespace my-workload shiftpv.io/admission=enabled
```

| Namespace | PV topology | Pod admission |
|---|---|---|
| Opt-in | 등록된 Pool node | owner pin 또는 Placement Hold |
| 일반 | 최초 owner만 | mobility webhook과 독립 |

Controller는 replica 하나와 `Recreate` strategy로 실행한다. 기존 node cordon을 관찰하고
[mobility preflight](../../docs/spec/volume-mobility.md#preflight-and-placement)를 거쳐 각 transaction을
`ShiftPVMove`에 기록한다.

ShiftPV는 Node를 cordon하지 않는다. Cordon은 cluster-wide maintenance 신호이므로 운영자가 다른
workload 영향과 maintenance window를 확인한다. Blocked 이동은 현재 owner를 검증하는
[ResumeOwner 절차](../../docs/spec/volume-mobility.md#blocked-recovery)로 복구한다.

### Webhook certificates

```mermaid
flowchart LR
    C[Controller] --> S[TLS Secret]
    S --> M[Mobility webhook]
    S --> L[Lifecycle webhook]
    C --> R[renew / rotate]
    R --> S
```

| Material | 유효 기간 | 교체 시점 |
|---|---:|---:|
| Serving certificate | 90 days | 30 days before expiry |
| CA | 10 years | 1 year before expiry |

Controller는 매분 certificate resource를 reconcile하고 Pod 재시작 없이 새 인증서를 제공한다. CA
교체는 trust bundle을 겹쳐 공개한 뒤 수렴한다. Secret 유실 시 webhook configuration의 active trust
root로 복구한다. 기간은 제품 상수다.

`mobility.enabled=false`는 HTTPS resource를 유지하고 `failurePolicy=Ignore`와 항상 false인 match
condition으로 placement admission을 inert 상태로 둔다.

## Upgrade

Upgrade와 rollback은 다음 조건을 유지하는 이동 트리거 동결 창에서 진행한다.

| 대상 | 교체 조건 |
|---|---|
| Volume | 모든 `activeMove`가 빈 값 |
| Identity | 모든 Volume에 `status.currentCopy`와 일치하는 node-side identity marker가 존재 |
| Move | 모두 `Succeeded` 또는 `Blocked` + `recoveryPhase=Recovered` |
| Cleanup | 모든 `ShiftPVCleanup`이 `Completed`인 상태 |
| 유지보수 창 | cordon·drain·수동 Move 요청을 동결하여 교체 조건 유지 |

`Completing`은 잠금 해제 뒤에도 남을 수 있는 미완료 journal이다. Move와 Cleanup이 종결된 뒤
Controller를 교체한다.
Identity 조건을 충족하지 않는 기존 volume은 자동 채택하지 않는다. workload별 data migration을
완료한 뒤 이 절차를 시작한다.
Helm은 설치된 CRD를 보존하므로 새 Controller보다 schema를 먼저 적용한다. `--force-conflicts`는
최초 Helm field ownership을 명시적으로 인수한다.

```mermaid
flowchart LR
    CRD[target CRD 적용] --> POOL[Pool schema 완성]
    POOL --> HELM[Helm upgrade]
    HELM --> READY[Controller + Node Ready 대기]
```

```bash
TARGET_CHART_VERSION=x.y.z
helm repo update shiftpv
helm show crds shiftpv/shiftpv --version "${TARGET_CHART_VERSION}" | \
  kubectl apply --server-side --field-manager=shiftpv-crds \
    --force-conflicts -f -
```

CRD 적용 뒤 runtime을 갱신한다.

```bash
helm upgrade shiftpv shiftpv/shiftpv \
  --version "${TARGET_CHART_VERSION}" \
  --namespace shiftpv-system --values shiftpv-values.yaml \
  --wait
```

Local checkout도 같은 순서로 `shiftpv/shiftpv --version "${TARGET_CHART_VERSION}"` 대신
`./charts/shiftpv`를 사용한다.

## Operate

```bash
kubectl get shiftpvpools
kubectl get shiftpvvolumes
kubectl get shiftpvmoves
kubectl get shiftpvcleanups
kubectl get shiftpvmove <move-name> -o yaml
kubectl get events -n default \
  --field-selector involvedObject.kind=ShiftPVMove,involvedObject.name=<move-name>
```

Cluster-scoped Move Event는 Kubernetes Event API의 `default` namespace에 기록된다. Status가 source of
truth이며 timestamp는 관측과 알림을 위한 값이다.

| 신호 | Source of truth |
|---|---|
| Pool health | `ShiftPVPool.status.conditions` |
| Volume authority | `ShiftPVVolume.status.ownerNode` |
| Move 진행과 운영 행동 | `ShiftPVMove.status` |
| Copy inventory | `ShiftPVPool.status.inventory` |
| 삭제 intent와 receipt | `ShiftPVCleanup.status` |
| 알림 | Kubernetes Events |
| Blocked owner 복구 | [ResumeOwner 절차](../../docs/spec/volume-mobility.md#blocked-recovery) |

복구 완료는 Move의 `recoveryPhase=Recovered`와 Volume의 `Ready`, 빈 `activeMove`로 판정한다.
Move의 원래 `phase=Blocked`와 실패 reason은 유지된다.

### Retire a retained volume

`Retain`은 PVC 삭제 뒤에도 PV, owner data와 reservation을 보존한다. 폐기는 선택한 PV의
reclaim policy를 `Delete`로 바꿔 CSI에 위임한다. [Kubernetes reclaim policy](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#reclaiming)를 따른다.

| 순서 | 운영자 확인 |
|---|---|
| 1. 폐기 대상 확정 | PV driver가 `csi.shiftpv.io`, claimRef UID와 대상 PVC UID 일치; 이미 Released면 이전 claimRef 확인 |
| 2. Move 수렴 | Volume `Ready`, 빈 `activeMove`; Blocked는 PVC가 존재할 때 ResumeOwner 완료 |
| 3. I/O 중지 | GitOps 원본에서 workload 중지, Pod 종료와 빈 `publishedNodes` 확인 |
| 4. 경로 확인 | 현재 `ownerNode`의 Ready Pool과 `mountPath/volumes/<volume-id>` 확인, 필요한 data 백업 |
| 5. 폐기 | 아래 명령으로 PV policy 변경, Bound PVC 삭제 |
| 6. 완료 확인 | Volume이 `Deleting`으로 publication을 차단한 뒤 Cleanup `Completed`; PV, owner directory, reservation ConfigMap, Volume CR 부재 |

확정한 PV 이름과 ShiftPV release namespace를 사용한다.

```bash
PV_NAME=pvc-confirmed-name
DRIVER_NAMESPACE=shiftpv-system
kubectl get pv "${PV_NAME}" -o yaml
VOLUME_ID=$(kubectl get pv "${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')
kubectl get shiftpvvolume "${VOLUME_ID}" -o yaml
kubectl -n "${DRIVER_NAMESPACE}" get configmap "${VOLUME_ID}" -o yaml
```

위 확인이 끝나면 실행한다. Released PV는 policy 변경 시 데이터 폐기가 시작된다.

```bash
kubectl patch pv "${PV_NAME}" --type=merge \
  -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
```

PVC가 아직 Bound이면 확인한 namespace와 이름으로 삭제한다. 이미 Released이면 이 단계를 생략한다.

```bash
kubectl -n <workload-namespace> delete pvc <confirmed-pvc-name> --wait=true
```

```bash
kubectl wait --for=delete "pv/${PV_NAME}" --timeout=5m
kubectl -n "${DRIVER_NAMESPACE}" wait --for=delete "configmap/${VOLUME_ID}" --timeout=5m
kubectl wait --for=delete "shiftpvvolume/${VOLUME_ID}" --timeout=5m
```

CSI는 immutable Cleanup intent → exact copy purge → receipt settlement → reservation → Volume CR 순서로
정리한다. Pool과 release는 완료까지 유지하고 PV finalizer는 Kubernetes/provisioner가 정리한다.
Timeout이면 Cleanup status, PV Event와 Controller/helper log를 함께 확인한다.

## Cleanup review

Cleanup reconciler는 mobility 설정과 독립적으로 실행된다. Node inventory가 exact copy와 실제 kubelet
publication을 관찰하고 Controller가 live API authority와 비교한다. 승인된 VolumeDelete/MoveSource는
helper가 실행하며, orphan과 불명확한 identity는 `NeedsReview`로 보존한다.

```bash
kubectl get shiftpvcleanups \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,REASON:.status.reason,NODE:.spec.target.nodeName,VOLUME:.spec.target.volumeID,COPY:.spec.target.copyID'
kubectl get shiftpvcleanup <cleanup-name> -o yaml
kubectl get shiftpvpool <pool-name> -o jsonpath='{.status.inventory}'
kubectl -n shiftpv-system logs job/<cleanup-name>-effect
```

| 결과 | 다음 동작 |
|---|---|
| `Pending` | 승인 intent와 Controller 상태 확인 |
| `Running` | status의 exact Job UID와 helper log 확인 |
| `Verifying` | receipt 정산 재시도 관찰 |
| `Completed` | 파일 효과와 정리 의무 종결; 동일 exact copy 재관측 시 `NeedsReview` 복구 |
| `NeedsReview` | data 보존; Pool/Volume/Move/copy identity를 운영자가 조사 |

Cleanup 대상·권한·예약 identity는 immutable이고 `approved`만 `false → true`로 변경할 수 있다. Orphan을
승인하기 전에 target이 current/in-flight copy가 아닌지, Pool inventory의 fresh/valid 상태,
`published=false`를 확인한다. cleanup이 orphan reservation도 회수할 때만 exact `reservationUID`를
가진다. 살아 있는 Volume의 reservation은 cleanup 대상에서 제외하며, 유효한 `currentCopy`가 target과
다른 exact copy임을 확인한다.

```bash
kubectl get shiftpvcleanup <cleanup-name> -o yaml
kubectl patch shiftpvcleanup <cleanup-name> --type merge -p '{"spec":{"approved":true}}'
```

승인 뒤에도 live 조건이 안전하지 않으면 data를 유지한 채 `NeedsReview` 사유만 갱신한다. 조건이
해소되면 아직 executor가 없는 요청은 같은 operation으로 자동 재개한다. executor가 이미 결합된
`NeedsReview`는 자동 재실행하지 않는다. Controller가 기록한 purged receipt만 `Completed`로 정산된다.
정산 뒤 동일 exact copy가 다시 나타나면 `CopyReappeared`로 보존하고 이전 삭제 권한은 재사용하지 않는다.
완료되지 않은 Cleanup은 Helm과 Argo CD uninstall을 차단한다.

## Uninstall and recovery

Uninstall guard와 lifecycle webhook이 하나의 제거 시도를 조정한다.

```mermaid
sequenceDiagram
    participant H as Helm / Argo CD
    participant G as Uninstall guard
    participant C as Controller
    participant W as Lifecycle webhook

    H->>G: PreDelete
    G->>C: quiescing (CSIDriver UID)
    C->>C: CreateVolume 종료 + 진행 호출 drain
    C-->>G: acknowledged
    G->>G: acknowledgement 이후 fresh Pool inventory 대기
    G->>G: dependency 검사
    G->>W: lifecycle validation 제거
    G-->>H: granted
    H->>H: release resource 제거
```

| Mode | 동작 |
|---|---|
| `helm` | 최대 90초의 bounded 검사; Pool inventory 갱신을 기다리고 blocker가 있으면 실패 반환 |
| `argocd` | Argo CD 3.3+ PreDelete가 dependency 해소까지 bounded 시도 반복 |

| Dependency | 제거 준비 상태 |
|---|---|
| 설정된 class를 사용하는 PVC | 해소 |
| ShiftPV PV | 해소 |
| controller namespace의 volume reservation | exact cleanup 뒤 해제 |
| `ShiftPVVolume` | 해소 |
| non-terminal `ShiftPVMove` | 해소 |
| controller namespace의 정리 요청 | 완료 acknowledgement 확인; 미완료·손상된 기록은 보존·확인 |
| 모든 Pool inventory | quiesce 이후 fresh·valid·complete이며 실제 copy 0개 |
| Kubernetes API 검사 | 성공 |

보호 대상은 labeled CSI Deployment, DaemonSet, Service, ServiceAccount, RBAC, StorageClass,
`CSIDriver`, ShiftPV CRD와 모든 `ShiftPVPool`, `ShiftPVVolume`, `ShiftPVMove`, `ShiftPVCleanup`이다. 정상 CSI
수명주기에서는 chart가 지정한 controller ServiceAccount만 Volume, Move, Cleanup을 정리한다.
Controller는 Pool에 protection finalizer를 설치한다. `kubectl delete shiftpvpool/<name>`은 Pool을
`PoolDeregistering`으로 전환해 신규 provisioning·이동 대상에서 제외한다. node inventory와 cleanup은
계속되며, deletion timestamp 이후의 최신·완전한 inventory가 비고 해당 exact Pool UID를 가리키는 PV,
reservation, Volume, 진행 중 Move, Cleanup이 없어지면 controller가 exact UID의 identity 해제를 승인한다.
Node는 filesystem lock 안에서 empty Pool을 재확인하고 identity marker를 해제해 상태로 보고하며,
controller가 이를 확인한 뒤 finalizer를 제거한다. 같은 빈 경로는 새 Pool UID로 다시 등록할 수 있다. 보호되지 않은 Pool
삭제, 외부 finalizer 제거와 그 밖의 직접 `kubectl delete`는 lifecycle admission이 차단한다. Admission은 read-only이므로
DELETE 또는 dry-run DELETE 자체가 제거 permit을 만들지 않는다.

정상 제거:

```text
Move 수렴 또는 owner 복구
  → GitOps/workload 중지
  → retained volume의 exact cleanup과 reservation 해제 확인
  → helm uninstall 또는 전용 Argo CD Application 삭제
```

Data 폐기는 [Retained volume 절차](#retire-a-retained-volume)를 따른다.

차단된 Helm 시도는 다음 log에서 확인한다.

```bash
kubectl -n <namespace> logs job/<release>-uninstall-guard
```

Helm은 pre-delete hook을 시작하기 전에 release를 `uninstalling`으로 기록한다. Guard가 차단한 release를
계속 운영하거나 upgrade하려면 차단 직전 revision을 hook 없이 복구한다.

```bash
helm history <release> --namespace <namespace>
helm rollback <release> <blocked-revision> --namespace <namespace> --no-hooks --wait
```

5분 동안 유지되는 quiescing/granted 상태는 현재 `CSIDriver` UID에 결합된다. 재설치는 새 UID와
새 lifecycle로 시작한다. 검사 실패는 quiescing을 취소하고 Controller reconciliation을 복구한다.
Argo CD mode는 5초 뒤 새 bounded 시도를 시작하고 Helm mode는 즉시 실패를 반환한다.
검사가 안전하게 끝나면 guard가 Pool protection finalizer를 exact UID 기준으로 해제한 뒤 chart 제거를
허용한다. 따라서 정상 uninstall은 CRD 밖에 dangling finalizer를 남기지 않는다.

Emergency recovery는 Helm hook 우회 전에 lifecycle admission을 명시적으로 제거한다.

```bash
kubectl get validatingwebhookconfiguration \
  -l app.kubernetes.io/managed-by=shiftpv-controller,app.kubernetes.io/component=lifecycle-admission
kubectl delete validatingwebhookconfiguration <name-from-above>
helm uninstall <release> --namespace <namespace> --no-hooks
```

Release 제거는 driver workload, RBAC, service, `CSIDriver`, chart-created StorageClass와
Controller-owned webhook resource를 처리한다. CRD, 사용자 PVC/PV, Pool/Volume/Move CR,
reservation과 host data는 독립 lifecycle을 유지한다. 복구는 같은 release namespace, Pool 선언과
mount path를 복원한 뒤 workload를 재개한다.

Argo CD에서는 ShiftPV를 전용 Application으로 관리하고 `lifecycle.uninstallMode=argocd`를 사용한다.
`PreDelete`는 Application 삭제에서 실행되며 일반 sync prune은 lifecycle admission만 적용된다.
