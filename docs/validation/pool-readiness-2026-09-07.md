# Pool readiness validation — 2026-09-07

## Scope

이 검증은 사전 마운트된 Pool의 실제 mount, write와 capacity 상태를 Node Plugin이
보고하고, Controller가 신규 provisioning과 cordon 이동을 fail-closed하는 경계를 확인한다.
물리 디스크 고장, source 영구 손실, 성능 부하와 backup/restore는 이 결과의 범위가 아니다.

## Static and unit result

`make verify`로 race test, statement coverage 80% gate, vet, build, release fixture,
shellcheck, Helm lint와 deterministic rendering을 실행했다. probe 단위 테스트는 missing path,
ordinary directory, permission denied, read-only, ENOSPC와 statfs 실패 reason을 고정한다.
Registry 테스트는 condition generation/freshness, node identity를 검증하고, 일시적으로
NotReady인 Pool도 향후 이동을 위한 PV accessible topology에는 남는 계약을 고정한다.

## Focused Kind result

Kubernetes 1.35.8의 두 worker에 서로 다른 host directory를 mount하고 다음 명령을 실행했다.

```bash
MOBILITY_FILESYSTEM_FAULTS_ONLY=1 \
  CLUSTER_NAME=shiftpv-pool-readiness \
  ./test/e2e/kind/run.sh
```

실제 관찰 결과는 다음과 같다.

- 두 Pool은 node-local probe 뒤 `Ready=True/PoolReady`가 되었고 정상 capacity admission을
  통과했다.
- destination mount를 inode/space가 고갈된 tmpfs로 가리자
  `Ready=False/NoSpace`가 되었으며 Move, volume lock과 staging이 생성되지 않았다.
- 같은 mount의 공간을 복구하자 `Ready=True`로 돌아왔고 Move가 자동 생성되어 checksum,
  PVC UID, PV와 volume handle을 유지한 채 destination owner로 완료됐다.
- 두 번째 Move의 copy Job 완료 뒤 Controller를 정지하고 destination을 read-only로
  remount했다. Node Plugin은 `Ready=False/ReadOnly`를 보고했고 재기동된 Controller는
  source owner와 검증된 staging을 보존한 채 `Copying/DestinationUnavailable`에서 대기했다.
- read-write remount 뒤 같은 Move가 promotion, owner commit과 source cleanup을 자동으로
  완료했고 workload 데이터 checksum이 유지됐다.
- runner는 exit zero로 끝났고 전용 Kind cluster와 임시 Pool directory를 제거했다.

최종 branch 상태는 이 snapshot이 아니라 동일 명령과 CI required checks를 다시 실행해
판단한다.
