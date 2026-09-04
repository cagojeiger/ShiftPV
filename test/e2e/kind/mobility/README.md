# Closed-loop mobility E2E

이 디렉터리는 ShiftPV 제품 Controller의 automatic cordon mobility를 격리된 kind
cluster에서 검증한다. 이동 진행을 위해 CR status를 직접 patch하거나 host-side rsync를
실행하지 않는다.

```bash
./test/e2e/kind/mobility/run.sh
```

테스트는 control-plane 1개와 서로 다른 host directory를 가진 worker 2개를 만들고
다음 사전 점검과 이동/복구 경로를 실행한다.

## Non-disruptive preflight

`preflight.sh`는 source-only hostname selector, required node affinity, destination
NoSchedule taint, PDB minAvailable=1을 각각 독립적으로 만든다. source cordon 뒤 두 번 이상
reconcile 간격 동안 같은 Pod UID, deletionTimestamp 없음, Volume Ready, Move 없음과 데이터
읽기/쓰기를 확인한다. selector는 controller 재시작 후에도 재검증한다.
PDB 제거 후에는 같은 volume의 자동 이동 성공과 checksum 유지까지 확인한다.
같은 namespace/PVC 이름을 재사용하되 삭제 후 남은 Retain PV/Volume은 별개 UID로 취급해야
한다. 각 시나리오와 전체 시험 끝에서 이전 volume에 새 Move/activeMove가 없고 Ready를
유지하는지도 확인한다. 특정 이동의 성공 메시지만으로 전체 검증을 통과시키지 않는다.

## Pre-commit failure and owner recovery

1. portable Deployment/PVC를 만든다.
2. source를 cordon하고 Move가 생성되면 해당 transaction의 destination staging 위치에
   테스트 파일을 만들어 mkdir를 실제 실패시킨다. CR status는 주입하지 않는다.
3. 정상 eviction/unpublish 후 copy가 실패하고 `Blocked/CopyFailed`로 끝나는지 확인한다.
4. dynamic owner와 source payload가 유지되고 destination final directory가 생기지 않았는지
   확인한다.
5. fault 파일 제거와 uncordon 후 `spec.recovery=ResumeOwner`를 두 번 요청한다. 잘못된 enum과 요청 제거는
   실제 CRD에서 거부해야 한다.
6. 복구 Verifying 중 Controller를 재시작하고 Recovered, 같은 owner/PVC UID/checksum과
   activeMove 해제를 확인한다. 원래 Move.phase=Blocked 이력은 유지해야 한다.

## Successful transaction and restart recovery

1. `WaitForFirstConsumer` PVC와 Deployment를 만들고 payload checksum을 기록한다.
   bound PVC의 Pod를 한 번 재생성해 admission의 owner hostname pin이 있는 상태도 검증한다.
2. owner source를 cordon한다.
3. 제품 Controller가 Move CR을 자동 생성하고 FSM을 진행하는지 관찰한다.
4. `Copying`, `Promoting`, `Committing` phase에서 Controller Pod를 각각 강제 삭제한다.
5. 재시작한 Controller가 기존 CR과 helper resource를 관찰해 같은 transaction을
   `Succeeded`까지 이어 가는지 확인한다.
6. replacement Pod가 destination에서 Running인지, PVC UID/PV/volume handle/checksum이
   같은지, dynamic owner가 destination인지 확인한다.
7. destination final payload와 source retired payload가 모두 있는지 확인한다.

## Post-commit failure and owner recovery

다시 반대 방향으로 이동시킨다. 테스트 전용 source pool의 `.shiftpv/retired/<move>`에 파일을
만들어 cleanup rename을 실제 실패시킨다. commit 이후 destination에서 새로운 payload를
기록하고 복구를 요청한다. 복구 중 Controller 재시작 후에도 최신 destination 데이터,
PVC/PV identity와 owner가 유지돼야 한다. 오래된 source는 aborted 경로에 보존 격리하며
source owner로 rollback하면 실패다. 자세한 실행 절차는 `recovery.sh`를 따른다.

합격 시 다음 두 메시지를 출력한다.

```text
ShiftPV blocked mobility E2E passed
ShiftPV closed-loop mobility E2E passed
```

`KEEP_CLUSTER=1`을 지정하면 실패 진단을 위해 cluster와 임시 pool directory를 남긴다.
기본값은 성공과 실패 모두 자동 정리다.
