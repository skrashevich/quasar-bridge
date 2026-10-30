package update

import "regexp"

const requestPrefix = `^\s*(?:алиса[,\s]+)?(?:пожалуйста[,\s]+)?(?:спроси\s+(?:нейросеть|свою\s+модель)[:,\s]+)?`
const checkRequest = `(?:проверь(?:те)?\s+(?:наличие\s+)?обновлени[яй]|есть\s+ли\s+(?:новые\s+)?обновления|доступны\s+ли\s+(?:новые\s+)?обновления)`
const installRequest = `установи(?:те)?\s+(?:обновлени[ея]|новую\s+версию)`
const requestTarget = `(?:\s+(?:для\s+)?(?:quasar[\s-]+bridge|квазар[\s-]+(?:бридж|мост)|(?:голосового\s+)?моста))?`
const requestSuffix = `(?:[,.\s!?]+пожалуйста)?[,.\s!?]*$`

// RequestPattern is shared by routing rules and read-only context prefetching.
// Match the whole request so another device, a negation, or a compound command
// cannot accidentally become a bridge update.
const RequestPattern = requestPrefix + `(?:` + checkRequest + `|` + installRequest + `)` + requestTarget + requestSuffix

var checkRequestPattern = regexp.MustCompile(`(?i)` + requestPrefix + checkRequest + requestTarget + requestSuffix)

func IsCheckRequest(text string) bool { return checkRequestPattern.MatchString(text) }
