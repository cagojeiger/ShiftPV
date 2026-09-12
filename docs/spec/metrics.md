# Metrics

## Data flow

```text
Node Pool probe + bounded copy inventory ── 메모리 ── /metrics
Controller           ── API snapshot ─ 메모리 ── /metrics
                     └─ CSI / discovery 완료 결과
```

| 경계 | 계약 |
|---|---|
| 활성화 | Helm `metrics.enabled`; 기본 false |
| Target 식별 | ServiceMonitor의 `shiftpv="true"`; dashboard query 범위를 ShiftPV로 한정 |
| 수집 요청 | 완료된 메모리 snapshot만 반환 |
| Node | 기존 Pool probe 주기 재사용; 등록 directory를 포함한 filesystem |
| Controller | 기본 30s마다 Pool·Volume·Move·Cleanup·reservation List; 한 pass timeout 10s |
| API 예산 | 전용 client 두 개가 2 QPS / burst 4 limiter를 공유; storage client 예산과 분리 |
| 저장소 판단 | 기존 admission 검사와 Move journal이 담당 |
| HTTP | 내부 HTTP `:8080/metrics`; webhook TLS와 별개 |

## Metric families

아래 이름에는 모두 `shiftpv_` 접두사가 붙는다.

| 이름 | 유형 | Label | 의미 |
|---|---|---|---|
| `pool_capacity_limit_bytes` | gauge | pool, node | 논리 예약 한도 |
| `pool_reserved_bytes` | gauge | pool, node | owner + 승인된 incoming 예약 합계 |
| `pool_unregistered_reserved_bytes` | gauge | pool, node | Volume CR 없는 예약 부분합; 생성 중 상태도 포함 |
| `pool_accounting_valid` | gauge | pool, node | 최근 예약 집계 유효성 0/1 |
| `pool_ready` | gauge | pool, node | snapshot 시점의 generation·probe freshness 적용 Ready |
| `pool_filesystem_size_bytes` | gauge | pool, node | directory를 포함한 filesystem 전체 크기 |
| `pool_filesystem_available_bytes` | gauge | pool, node | statfs BAvail 기반 여유; 외부 writer 사용량 포함 |
| `pool_filesystem_available_inodes` | gauge | pool, node | statfs Ffree 기반 inode 여유 |
| `pool_inventory_valid` | gauge | pool, node | 최신 bounded node inventory의 유효성 0/1 |
| `pool_inventory_truncated` | gauge | pool, node | 고정 관찰 상한 초과 여부 0/1 |
| `metrics_snapshot_success` | gauge | source | 최근 관찰 성공 0/1 |
| `metrics_snapshot_last_success_timestamp_seconds` | gauge | source | 마지막 성공 Unix 시각; 첫 성공 전 0 |
| `volumes` | gauge | phase | 현재 Volume CR 수 |
| `moves` | gauge | phase | 현재 Volume의 activeMove로 연결된 Move와 미완료 Completing 수; 중복 제외 |
| `cleanup_requests` | gauge | state | Pending·Running·Verifying·NeedsReview·Completed·Unknown 정리 요청 수 |
| `copy_observations` | gauge | state | Current·InFlight·CleanupTarget·OrphanPreserved·Missing·NeedsReview copy 수 |
| `mobility_deferred_volumes` | gauge | reason | 완료된 cordon discovery에서 보류된 Volume 수 |
| `csi_requests_total` | counter | method, code | CSI lifecycle RPC 완료 호출 수; 재시도 포함 |
| `csi_request_duration_seconds` | histogram | method | 해당 RPC 처리 시간 |

## Interpretation

| 상황 | 관측 계약 |
|---|---|
| `source` | Controller: metadata, mobility 활성 시 discovery; Node: filesystem |
| API 또는 probe 오류 | 마지막 성공 값·시각 유지, success=0 |
| 아직 관찰하지 못한 capacity | 수치 시계열 미발행 |
| 예약 집계 오류 | 해당 Pool accounting_valid=0, 마지막 정상 예약 수치 유지 |
| Pool 삭제 확인 | 다음 성공 관찰에서 해당 Pool 시계열 제거 |
| Node Pool 미등록 | filesystem 시계열 제거, success=0, 마지막 성공 시각 유지 |
| Volume 없는 예약 | 논리 예약으로 계수; 삭제 여부는 별도 운영 판단 |
| API owner 없는 exact copy | `OrphanPreserved`; unapproved cleanup으로 보존 |
| API current/in-flight copy가 inventory에 없음 | `Missing` |
| 손상 marker·unrecorded path | `NeedsReview` |
| inventory 상한 초과 | truncated=1; 보이지 않은 copy를 Missing/absent 증거로 사용하지 않음 |
| Recovered 이력 | activeMove 연결이 해제된 종결 Move는 현재 Move 수에서 제외 |
| 완료 기록 재시도 | Completing은 잠금 해제·Volume 삭제 뒤에도 종결까지 집계 |
| 잘못된 activeMove 연결 | metadata snapshot 실패로 표시 |
| mobility 비활성화 | discovery 시계열 미발행 |
| listener 실패 | 오류 log 기록; CSI process와 readiness 유지 |
| process 재시작 | CSI counter 초기화; snapshot은 첫 관찰부터 재구성 |

예약량은 파일 크기가 아니다. Filesystem 여유는 같은 filesystem을 공유하는 모든 사용자의 영향을
받는다. `size - available`에는 OS 예약 block도 포함될 수 있다. 실제 할당은 기존 fresh admission
검사가 결정한다.

`phase`, `reason`은 코드의 고정 enum이며 알 수 없는 값은 `Unknown`으로 집계한다.
CSI method는 CreateVolume, DeleteVolume, NodePublishVolume, NodeUnpublishVolume 네 개다.
code는 gRPC status code다. 식별자·경로·오류 원문은 CR/Event/log에서 찾는다.

## Freshness

```promql
time() - (shiftpv_metrics_snapshot_last_success_timestamp_seconds{source="filesystem"} > 0)
shiftpv_metrics_snapshot_success == 0
shiftpv_pool_accounting_valid == 0
```

success=1은 마지막 작업 성공을 뜻한다. 작업 정지나 장시간 지연은 마지막 성공 시각의 경과로
판단한다. 서로 다른 process의 값은 관찰 시점도 다르다. 동일 filesystem을 가리키는 여러 Pool의
filesystem 수치를 합산하면 중복될 수 있다.

설정과 접근 범위는 [Helm guide](../../charts/shiftpv/README.md#metrics)가 소유한다.

## Alerts

`metrics.prometheusRule.enabled=true`는 Prometheus Operator의 `PrometheusRule`을 만든다. 모든 규칙은
`shiftpv="true"` target label만 평가하고 조건이 5분 지속될 때 firing된다. 값이 정상으로 돌아오면
다음 평가에서 해제된다.

| Alert | 조건 |
|---|---|
| `ShiftPVObservationFailed` | 최근 snapshot 작업 실패 |
| `ShiftPVObservationStale` | 마지막 성공이 5분보다 오래됨, 관측 시계열이 없음, 또는 metrics target 수집 실패 |
| `ShiftPVPoolAccountingInvalid` | 예약 회계를 capacity 판단에 사용할 수 없음 |
| `ShiftPVPoolInventoryUnsafe` | copy inventory가 invalid 또는 truncated |
| `ShiftPVCleanupNeedsReview` | 운영자 확인을 기다리는 Cleanup 존재 |
| `ShiftPVCopyNeedsReview` | orphan-preserved, missing, unsafe identity copy 존재 |

Prometheus가 ServiceMonitor 없이 직접 수집할 때도 target에 `shiftpv="true"` label을 추가한다. Alert는
삭제 명령이 아니라 관찰 결과이며, 대상 CR과 Pool inventory를 확인한 뒤 조치한다.
