-- Argo CD health check for Ambit objects (Model, Server, AccessRule): Healthy when the Ambit controller
-- reports Ready=True for the current generation, Degraded on Ready=False, Progressing otherwise.
local hs = { status = "Progressing", message = "Waiting for the Ambit controller" }
if obj.status == nil or obj.status.conditions == nil then
  return hs
end
for _, c in ipairs(obj.status.conditions) do
  if c.type == "Ready" then
    if c.observedGeneration ~= nil and obj.metadata.generation ~= nil and c.observedGeneration < obj.metadata.generation then
      hs.message = "Waiting for the Ambit controller to see the latest change"
      return hs
    end
    if c.status == "True" then
      hs.status = "Healthy"
    elseif c.status == "False" then
      hs.status = "Degraded"
    end
    hs.message = c.message
  end
end
return hs
