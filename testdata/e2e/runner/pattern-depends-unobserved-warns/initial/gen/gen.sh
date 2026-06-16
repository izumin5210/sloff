# Emit one copy task per *.src file (per-source fan-out, ADR-0015). Outputs use
# .txt because the repo .gitignore excludes *.out from the committed golden.
printf '{"schema_version":"v1","tasks":['
sep=
for f in *.src; do
  [ -e "$f" ] || continue
  name=${f%.src}
  printf '%s{"name":"copy-%s","cmd":["cp","%s","%s.txt"],"inputs":["%s"],"outputs":["%s.txt"],"tools":["versioner"]}' "$sep" "$name" "$f" "$name" "$f" "$name"
  sep=,
done
printf ']}'
