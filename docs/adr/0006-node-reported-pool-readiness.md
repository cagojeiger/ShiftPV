# 0006. Node가 실제 Pool readiness를 보고한다

- 상태: Accepted
- 날짜: 2026-09-07
- 상세 계약: [CSI driver](../spec/csi-driver.md), [Volume mobility](../spec/volume-mobility.md)

## Context

API admission은 node의 Pool 경로가 실제 directory인지, 쓰기 가능한지, filesystem capacity를 읽을
수 있는지 확인할 수 없다. ShiftPV는 별도 mount와 root filesystem 하위 directory를 모두 지원한다.

## Decision

Node Plugin이 자기 node의 Pool을 주기적으로 검사하고 표준 Conditions로 상태를 보고한다.

| Condition | 증거 |
|---|---|
| `Accessible` | 기존 directory에 접근 가능 |
| `Writable` | 임시 쓰기, sync, 정리 성공 |
| `CapacityReadable` | filesystem capacity 조회 성공 |
| `Ready` | 모든 조건과 freshness 충족 |

최근 `Ready` Pool만 신규 provisioning과 이동 candidate가 된다. 기존 volume의 owner authority는
`ShiftPVVolume`이 계속 결정한다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| CR admission 검사 | node filesystem에 접근할 수 없다. |
| exact mount point 검사 | 일반 directory Pool을 제외한다. |
| 첫 PVC 요청 시 검사 | 등록 오류를 사전에 발견하기 어렵다. |

## Consequences

운영자는 Conditions에서 경로 상태와 실패 이유를 본다. Node Plugin 중단 시 마지막 상태는 freshness
기한 뒤 신규 storage action에서 제외된다. Probe interval이 장애 감지 속도와 작은 filesystem I/O
비용을 조절한다.
