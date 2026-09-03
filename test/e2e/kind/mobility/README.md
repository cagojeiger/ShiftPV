# Closed-loop mobility E2E

이 디렉터리는 ShiftPV 제품 Controller의 automatic cordon mobility를 격리된 kind
cluster에서 검증한다. 이동 진행을 위해 CR status를 직접 patch하거나 host-side rsync를
실행하지 않는다.

```bash
./test/e2e/kind/mobility/run.sh
```

테스트는 control-plane 1개와 서로 다른 host directory를 가진 worker 2개를 만들고
다음 두 transaction을 실행한다.

## Blocked transaction

1. source node 전용 hostname selector가 있는 Deployment와 ShiftPV PVC를 만든다.
2. source를 cordon한다.
3. Controller가 volume을 잠그고 consumer를 evict한 뒤 replacement Pod의 Placement Hold를
   해제한다.
4. selector가 destination candidate와 충돌해 move가
   `Blocked/UnsupportedSchedulingConstraint`로 끝나는지 확인한다.
5. dynamic owner와 source payload가 유지되고 destination final directory가 생기지 않았는지
   확인한다.

## Successful transaction and restart recovery

1. `WaitForFirstConsumer` PVC와 Deployment를 만들고 payload checksum을 기록한다.
2. owner source를 cordon한다.
3. 제품 Controller가 Move CR을 자동 생성하고 FSM을 진행하는지 관찰한다.
4. `Copying`과 `Committing` phase에서 Controller Pod를 각각 강제 삭제한다.
5. 재시작한 Controller가 기존 CR과 helper resource를 관찰해 같은 transaction을
   `Succeeded`까지 이어 가는지 확인한다.
6. replacement Pod가 destination에서 Running인지, PVC UID/PV/volume handle/checksum이
   같은지, dynamic owner가 destination인지 확인한다.
7. destination final payload와 source retired payload가 모두 있는지 확인한다.

합격 시 다음 두 메시지를 출력한다.

```text
ShiftPV blocked mobility E2E passed
ShiftPV closed-loop mobility E2E passed
```

`KEEP_CLUSTER=1`을 지정하면 실패 진단을 위해 cluster와 임시 pool directory를 남긴다.
기본값은 성공과 실패 모두 자동 정리다.
