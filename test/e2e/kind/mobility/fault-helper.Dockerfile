FROM shiftpv:dev

USER 0:0
RUN mv /shiftpv-volume-helper /shiftpv-volume-helper-real
COPY fault-helper.sh /shiftpv-volume-helper
RUN chmod 0755 /shiftpv-volume-helper
USER 65532:65532
