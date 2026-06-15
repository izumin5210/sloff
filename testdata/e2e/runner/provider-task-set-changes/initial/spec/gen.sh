# Emit one copy task per *.src file in this dir. The set of tasks is derived
# from the filesystem (per-source fan-out), not hand-written in sloff.yml.
printf '{"schema_version":"v1","tasks":['
sep=
for f in *.src; do
  [ -e "$f" ] || continue
  name=${f%.src}
  printf '%s{"name":"copy-%s","cmd":["cp","%s","%s.out"],"inputs":["%s"],"outputs":["%s.out"],"tools":["versioner"]}' "$sep" "$name" "$f" "$name" "$f" "$name"
  sep=,
done
printf ']}'
