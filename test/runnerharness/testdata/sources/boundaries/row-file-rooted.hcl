schema "public" {}
table "regions" {
  schema = schema.public
  column "code" {
    null = false
    type = text
  }
  column "name" {
    null = true
    type = text
  }
  primary_key {
    columns = [column.code]
  }
}
data {
  table = table.regions
  keys  = ["code"]
  file  = "/proc/self/environ"
}
