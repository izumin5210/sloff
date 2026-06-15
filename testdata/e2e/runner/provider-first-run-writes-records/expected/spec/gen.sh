# Emit one copy task per *.src file in this dir. The set of tasks is derived
# from the filesystem (per-source fan-out), not hand-written in sloff.yml.
# Outputs use .txt (not .out) because the repo .gitignore excludes *.out, which
# would keep the generated golden outputs out of the committed expected/ tree.
printf '{"schema_version":"v1","tasks":['
sep=
for f in *.src; do
  [ -e "$f" ] || continue
  name=${f%.src}
  printf '%s{"name":"copy-%s","cmd":["cp","%s","%s.txt"],"inputs":["%s"],"outputs":["%s.txt"],"tools":["versioner"]}' "$sep" "$name" "$f" "$name" "$f" "$name"
  sep=,
done
printf ']}'
