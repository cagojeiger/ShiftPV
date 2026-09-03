# 0007. admission 인증서는 Controller가 조정하고 무중단 갱신한다

- 상태: Accepted
- 날짜: 2026-09-03

## Context

automatic mobility의 admission webhook은 Kubernetes API server와 HTTPS로 통신한다.
서버 인증서의 DNS SAN은 chart가 생성한 `<service>.<namespace>.svc` 이름과 일치해야 하고,
`MutatingWebhookConfiguration.clientConfig.caBundle`은 그 인증서를 발급한 CA를 신뢰해야
한다.

Helm의 `genCA`와 `genSignedCert`로 설치 시점에 인증서를 생성하면 렌더 결과가 매번
달라지고, 만료 전 갱신과 실행 중인 HTTPS 서버의 reload를 닫힌 루프로 만들 수 없다.
별도 certificate operator를 필수 dependency로 두는 것도 현재 소중규모 제품 범위보다
크다.

## Decision

- 인증서 갱신이나 mobility 비활성화를 위한 별도 CronJob, sidecar, hook Job, 외부
  certificate operator를 추가하지 않는다.
- 기존 Controller process의 certificate reconciler가 다음 리소스를 소유하고 1분마다
  실제 상태를 원하는 상태로 수렴시킨다.
  - namespace의 TLS Secret: `ca.crt`, `ca.key`, `tls.crt`, `tls.key`
  - mobility `MutatingWebhookConfiguration`, lifecycle
    `ValidatingWebhookConfiguration`과 각각의 `caBundle`
- serving certificate는 90일 유효하며 만료 30일 전 새 key와 certificate로 갱신한다.
- CA는 10년 유효하며 만료 1년 전 새 CA로 교체한다. CA 교체 순서는
  `old+new caBundle -> 새 Secret과 HTTPS certificate -> new caBundle`이다.
- Secret이 유실되거나 손상되어도 기존 `MutatingWebhookConfiguration.caBundle`에서
  현재 trust root를 복구하여 같은 dual-CA 전환 순서를 사용한다.
- HTTPS server는 `tls.Config.GetCertificate`로 reconciler가 보관한 최신 certificate를
  handshake마다 읽는다. Pod restart나 파일 remount 없이 갱신이 적용된다.
- Controller 시작 시 certificate와 webhook 구성이 먼저 수렴한 뒤 HTTPS readiness가
  열리며, Helm 리소스 적용 순서 차이는 제한된 재시도로 흡수한다.
- Helm은 webhook Service만 선언한다. 무작위 certificate bytes를 chart manifest에
  넣지 않아 같은 values의 렌더 결과를 결정적으로 유지한다.
- Secret은 webhook Service를 owner로, 두 webhook configuration은 `CSIDriver`를 owner로
  둔다. Controller는 update 전에 managed label과 예상 owner API version/kind/name/UID를
  모두 확인하며, 같은 이름의 외부 resource나 이전 설치의 resource를 덮어쓰지 않는다.
- `mobility.enabled=false`에서도 Service, HTTPS server, Secret과 webhook configuration을
  유지한다. 단, webhook에는 항상 false인 match condition을 넣고
  `failurePolicy=Ignore`로 바꿔 API server가 endpoint를 호출하지 않는 inert 상태로 둔다.
  다시 활성화하면 false condition을 제거하고 `failurePolicy=Fail`로 복구한다.
- lifecycle validation webhook은 mobility 설정과 무관하게 항상 fail-closed로 유지한다.
  두 webhook은 같은 serving certificate와 CA transition bundle을 사용한다.
- 정상 Helm uninstall 또는 Argo CD Application 삭제에서는 Controller가 uninstall
  `quiescing` 상태를 관찰한 뒤 lifecycle validation reconcile을 중지한다. Controller가
  진행 중인 provisioning의 drain을 확인해 acknowledgement를 남긴 뒤에만 guard가
  `ValidatingWebhookConfiguration`을 제거한다. TLS Secret과 mobility webhook은 owner
  resource 삭제에 따른 Kubernetes garbage collection으로 제거된다.

## Consequences

- Controller ServiceAccount에는 Secret과 두 webhook configuration을 조정할 권한이
  필요하다.
- mobility 비활성화는 resource 삭제 순서나 별도 hook ServiceAccount에 의존하지 않는다.
- uninstall quiesce가 취소되거나 만료되면 lifecycle validation reconcile이 자동으로
  재개된다.
- CA private key는 Controller namespace의 Secret에 저장되므로 해당 namespace의 Secret
  읽기 권한은 storage operator 경계로 취급해야 한다.
- Controller와 Kubernetes API가 갱신 기간 전체에 걸쳐 계속 중단되면 자동 갱신할 수
  없다. 복구 후 다음 startup reconcile 또는 주기 reconcile에서 다시 수렴한다.
- 현재 certificate 기간은 제품 내부 계약이며 Helm value로 노출하지 않는다.
