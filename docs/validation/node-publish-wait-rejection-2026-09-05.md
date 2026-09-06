# NodePublish wait experiment rejection — 2026-09-05

> Historical experiment only. `test/performance/evaluate-home-mobility.sh` validates
> this rejected experiment's captured evidence and intentionally requires
> `SHIFTPV_REJECTED_NODE_PUBLISH_WAIT_EXPERIMENT=1`. It is not a release or
> readiness gate for the current reservation-Pod design.

## Scope

Home mobility baseline에서 `WaitingForDestinationPublish`가 31.527초였고 kubelet의
volume-operation backoff가 지배했다. `Moving` 상태의 `NodePublishVolume`을 호출 context
안에서 기다리게 하면 이 구간이 2.631초로 줄어드는 A/B 결과가 있었다. 고정 500ms GET을
제거하기 위해 resourceVersion 기반 per-request watch를 구현하고 다음 반증 검증을 수행했다.

## Static and unit result

- 정상 `Ready`: GET 1회, watch 0회
- `Moving`: GET 1회 뒤 Volume 이름에 한정한 watch 1회
- 12개 동시 대기: GET 12회, watch 12회이며 유휴 구간 추가 GET 없음
- watch channel 종료와 resourceVersion 만료: GET 재동기화 후 재연결
- `Blocked`, owner 불일치, 삭제, API 오류와 context 만료: mount 없이 fail-closed
- `make verify`: 통과, 전체 statement coverage 83.8%

이 결과는 API polling 부하와 상태 전이 정확성만 증명했다. node process와 kubelet의
재시작 경계까지 증명하지 못한다.

## Kind result

새 소스 image와 chart를 사용한 전체 closed-loop mobility E2E는 통과했다.

```bash
CLUSTER_NAME=shiftpv-watch-e2e ./test/e2e/kind/mobility/run.sh
```

copy 실패와 source-owner 복구, 정상 양방향 이동, Copying/Promoting/Committing controller
재시작, owner와 checksum, certificate reconcile, mobility disable을 확인했다. 별도로 owner
commit 뒤 destination node를 실제 stop/start한 사례도 다음 checksum으로 통과했다.

```text
phase: WaitingForDestinationPublish
result: Succeeded
checksum: 09def66b59517b3e7623ecd0a8bdd3d2fe7399dca3f617148e7142aefb5f2e91
```

## Counterexample

destination 선택 뒤 copy 전에 node container를 stop/start하고 `ResumeOwner`를 요청하는 기존
검증은 두 번 연속 300초 안에 `Recovered`가 되지 않았다. timeout을 600초로 늘린 세 번째
재현도 수렴하지 않아 중단했다.

관찰 상태는 다음과 같았다.

- Volume은 source-owned `Ready`였고 source replacement Pod와 원본 checksum은 정상이었다.
- destination의 기존 replacement Pod는 컨테이너가 시작되지 않은 `ContainerCreating` 상태로
  삭제 요청 뒤에도 `Terminating`에 남았다.
- destination kubelet은 해당 Pod의 CSI target에 `vol_data.json`이 없어서 unmounter를 만들지
  못한다는 오류를 약 100ms 간격으로 반복했다.
- Node Plugin은 재등록됐고 `Blocked`, 이후 source owner 불일치를 fail-closed로 반환했다.
- `publishedNodes`에 destination publication은 없었고 잘못된 mount나 data authority 전환은
  관찰되지 않았다.

핵심 오류는 다음 형태였다.

```text
UnmountVolume.NewUnmounter failed ...
failed to open volume data file .../vol_data.json: no such file or directory
```

이는 `NodePublishVolume`이 `Moving`을 기다리는 동안 node가 중단되면 kubelet이 성공한 publish의
metadata 없이 incomplete volume directory를 복구 대상으로 남길 수 있음을 보여준다. 데이터
권위는 안전했지만 Pod와 Move recovery가 닫힌 루프로 수렴하지 않았으므로 제품 허용 조건을
충족하지 않는다.

## Decision

고정 polling과 per-request watch를 포함해 CSI `NodePublishVolume` 호출 안에서 `Moving → Ready`
전이를 기다리는 전략을 폐기한다. Node Plugin은 현재 계약대로 `Moving`을 즉시 fail-closed한다.
후속 구현은 CSI 요청을 늘리지 않고 실제 workload를 scheduling gate 아래 둔 채 별도 placement
reservation을 scheduler에 제출한다. owner commit 뒤에만 workload를 release하므로 지원되는
자동 이동 경로에서 이 반증의 incomplete target과 publish backoff를 피한다. 현재 검증은
[event-driven placement 기록](event-driven-placement-2026-09-05.md)에 둔다.

실패 경계를 독립 재현할 수 있도록 `MOBILITY_NODE_RESTART_CASE` 선택자를 test harness에
추가했다. `destination`은 위 반증을, `committed-destination`은 commit 이후 자동 연속성을
각각 실행한다. 모든 임시 Kind 클러스터는 종료 시 제거했다.
