# Reads `tofu show -json <planfile>`.
#   --arg mode counts   -> {"create": n, "update": n, ...}
#   --arg mode <kind>   -> one line per resource of that kind

def kind:
  .change.actions as $a
  | if $a == ["no-op"] then "noop"
    elif $a == ["read"] then "read"
    elif $a == ["create"] then "create"
    elif $a == ["update"] then "update"
    elif $a == ["delete"] then "delete"
    elif ($a | index("delete")) and ($a | index("create")) then "replace"
    else "other" end;

# Attribute names only, never values, so nothing sensitive reaches a PR comment.
def changed_attrs:
  (.change.before // {}) as $b
  | (.change.after // {}) as $a
  | [($a | keys[]?), ($b | keys[]?)] | unique | map(select($b[.] != $a[.]));

def detail:
  if kind == "update" and (changed_attrs | length) > 0 then
    "  (" + (changed_attrs[:8] | join(", ")) + (if (changed_attrs | length) > 8 then ", …" else "" end) + ")"
  elif (.action_reason // "") != "" then
    "  (" + .action_reason + ")"
  else "" end;

if $mode == "counts" then
  (.resource_changes // []) | map(kind) | group_by(.) | map({(.[0]): length}) | add // {}
else
  {"create": "+", "update": "~", "delete": "-", "replace": "-/+", "other": "?"}[$mode] as $sym
  | (.resource_changes // [])[] | select(kind == $mode)
  | "\($sym) \(.address)" + detail
end
