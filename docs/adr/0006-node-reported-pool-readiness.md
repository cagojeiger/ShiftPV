# 0006. Node가 실제 Pool readiness를 보고한다

- 상태: Accepted
- 날짜: 2026-09-07
- 적용 범위: 0.4 target contract
- 상세 계약: [CSI driver](../spec/csi-driver.md), [Volume mobility](../spec/volume-mobility.md)

## Context

API admission은 node의 Pool 경로가 실제 directory인지, 쓰기 가능한지, filesystem capacity를 읽을
수 있는지 확인할 수 없다. 이동과 cleanup은 stale scan이 아니라 특정 요청 이후의 complete observation에
의존해야 한다.

## Decision

Node Plugin이 자기 node의 Pool을 주기적으로 검사하고 표준 Conditions와 bounded inventory를 보고한다.
신규 allocation과 destination 판정은 Node의 `status.observedGeneration`이 현재 `metadata.generation`과
같으며 inventory가 valid·non-truncated인 Ready Pool만 사용한다. Cleanup receipt 후 exact absence가
필요하면 Controller가 `spec.scanEpoch`를 증가시켜 새 generation을 요청하고 그 generation의
valid·complete inventory만 causal proof로 사용한다.

| Condition | 증거 |
|---|---|
| `Accessible` | 기존 directory에 접근 가능 |
| `Writable` | 임시 쓰기, sync, 정리 성공 |
| `CapacityReadable` | filesystem capacity 조회 성공 |
| `Ready` | 위 세 Condition과 current generation, freshness 충족 |

Inventory completeness는 별도 Condition이 아니라 `status.inventory.valid`, `truncated`, `message`로
판정한다. Rsync metadata compatibility는 Node readiness가 아니라 배포 전 filesystem acceptance
probe에서 확인한다.

최근 `Ready` Pool만 신규 provisioning과 이동 candidate가 된다. 기존 volume의 owner authority는
`ShiftPVVolume`이 계속 결정한다. Truncated, invalid, missing identity, 또는 conflicting observation은
storage action의 근거가 아니며 destructive action을 멈춘다.

Device node 재생성과 nested filesystem traversal은 이동 data contract에서 제외한다. 계약에 포함된
metadata를 보존할 수 없는 filesystem 조합은 운영 대상으로 승인하지 않는다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| CR admission 검사 | node filesystem에 접근할 수 없다. |
| exact mount point 검사 | 일반 directory Pool을 제외한다. |
| 첫 PVC 요청 시 검사 | 등록 오류를 사전에 발견하기 어렵다. |
| wall-clock freshness만 사용 | Controller 요청과 Node 관측 사이의 causality를 증명하지 못한다. |

## Consequences

운영자는 Conditions에서 경로 상태와 실패 이유를 본다. Node Plugin 중단 시 마지막 상태는 freshness
기한 뒤 신규 storage action에서 제외된다. Generation fence는 stale success를 막지만, observation을
요구할 때마다 Pool spec update와 Node write가 필요하다.
