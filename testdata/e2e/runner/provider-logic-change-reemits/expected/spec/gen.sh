printf '{"schema_version":"v1","tasks":['
sep=
for f in *.src; do
  [ -e "$f" ] || continue
  name=${f%.src}
  if [ "$name" = a ]; then
    printf '%s{"name":"copy-%s","cmd":["sh","-c","cp %s %s.txt; printf v2 >> %s.txt"],"inputs":["%s"],"outputs":["%s.txt"],"tools":["versioner"]}' "$sep" "$name" "$f" "$name" "$name" "$f" "$name"
  else
    printf '%s{"name":"copy-%s","cmd":["cp","%s","%s.txt"],"inputs":["%s"],"outputs":["%s.txt"],"tools":["versioner"]}' "$sep" "$name" "$f" "$name" "$f" "$name"
  fi
  sep=,
done
printf ']}'
