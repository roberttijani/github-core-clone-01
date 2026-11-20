package main

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
)

var V2UploadPackAdvertisement []byte

func init() {
	var buf bytes.Buffer
	buf.Write(EncodePktString("version 2\n"))
	buf.Write(EncodePktString("agent=makefun-forge/1.0\n"))
	buf.Write(EncodePktString("ls-refs=peel symrefs unborn\n"))
	buf.Write(EncodePktString("fetch=shallow wait-for-done filter\n"))
	buf.Write(EncodePktString("server-option\n"))
	buf.Write(EncodePktString("object-format=sha1\n"))
	buf.Write(PktFlushBytes)
	V2UploadPackAdvertisement = buf.Bytes()
}

func handleGitHTTP(w http.ResponseWriter, r *http.Request, store *ForgeStore, owner, repoName, gitAction string) {
	repo := store.GetRepo(owner, repoName)
	if repo == nil || strings.ToLower(owner) != strings.ToLower(store.Viewer.Login) {
		sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
		return
	}

	repo.Mu.Lock()
	defer repo.Mu.Unlock()

	// GET info/refs
	if gitAction == "info/refs" && r.Method == http.MethodGet {
		service := r.URL.Query().Get("service")
		gitProto := r.Header.Get("Git-Protocol")

		if service == "git-upload-pack" {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			if gitProto == "version=2" {
				w.WriteHeader(http.StatusOK)
				w.Write(V2UploadPackAdvertisement)
				return
			}

			// v0 upload-pack
			var buf bytes.Buffer
			buf.Write(EncodePktString("# service=git-upload-pack\n"))
			buf.Write(PktFlushBytes)
			caps := "multi_ack_detailed thin-pack side-band side-band-64k ofs-delta shallow agent=makefun-forge/1.0"
			mainOid, hasMain := repo.Refs["refs/heads/main"]
			if len(repo.Refs) == 0 || !hasMain {
				buf.Write(EncodePktString(ZeroOID + " capabilities^{}\x00" + caps + "\n"))
			} else {
				first := true
				// Write main first if present
				buf.Write(EncodePktString(mainOid + " refs/heads/main\x00" + caps + "\n"))
				for refName, oid := range repo.Refs {
					if refName == "refs/heads/main" {
						continue
					}
					buf.Write(EncodePktString(oid + " " + refName + "\n"))
				}
				_ = first
			}
			buf.Write(PktFlushBytes)
			w.WriteHeader(http.StatusOK)
			w.Write(buf.Bytes())
			return
		} else if service == "git-receive-pack" {
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			var buf bytes.Buffer
			buf.Write(EncodePktString("# service=git-receive-pack\n"))
			buf.Write(PktFlushBytes)
			caps := "report-status side-band-64k delete-refs ofs-delta agent=makefun-forge/1.0"
			if len(repo.Refs) == 0 {
				buf.Write(EncodePktString(ZeroOID + " capabilities^{}\x00" + caps + "\n"))
			} else {
				first := true
				for refName, oid := range repo.Refs {
					if first {
						buf.Write(EncodePktString(oid + " " + refName + "\x00" + caps + "\n"))
						first = false
					} else {
						buf.Write(EncodePktString(oid + " " + refName + "\n"))
					}
				}
			}
			buf.Write(PktFlushBytes)
			w.WriteHeader(http.StatusOK)
			w.Write(buf.Bytes())
			return
		} else {
			sendJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid service"}, nil)
			return
		}
	}

	// POST git-upload-pack
	if gitAction == "git-upload-pack" && r.Method == http.MethodPost {
		reqBytes, _ := io.ReadAll(r.Body)
		packets := DecodePktLines(reqBytes)
		lines := PacketsToStringLines(packets)
		gitProto := r.Header.Get("Git-Protocol")

		isV2 := gitProto == "version=2" || (len(lines) > 0 && strings.HasPrefix(lines[0], "command="))
		if isV2 {
			command := ""
			for _, l := range lines {
				if strings.HasPrefix(l, "command=") {
					command = strings.TrimSpace(strings.TrimPrefix(l, "command="))
					break
				}
			}

			if command == "ls-refs" {
				symrefs := false
				peel := false
				var prefixes []string
				for _, l := range lines {
					t := strings.TrimSpace(l)
					if t == "symrefs" {
						symrefs = true
					} else if t == "peel" {
						peel = true
					} else if strings.HasPrefix(l, "ref-prefix ") {
						prefixes = append(prefixes, strings.TrimSpace(l[11:]))
					}
				}

				cacheKey := strconv.FormatBool(symrefs) + ":" + strconv.FormatBool(peel) + ":" + strings.Join(prefixes, ",")
				if cached, ok := repo.LsRefsCache[cacheKey]; ok {
					w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
					w.Header().Set("Content-Length", strconv.Itoa(len(cached)))
					w.WriteHeader(http.StatusOK)
					w.Write(cached)
					return
				}

				var buf bytes.Buffer
				defaultBranch := repo.DefaultBranch
				if defaultBranch == "" {
					defaultBranch = "main"
				}
				headOid := repo.Refs["refs/heads/"+defaultBranch]

				matchesPrefix := func(name string) bool {
					if len(prefixes) == 0 {
						return true
					}
					for _, p := range prefixes {
						if strings.HasPrefix(name, p) {
							return true
						}
					}
					return false
				}

				if len(repo.Refs) == 0 {
					buf.Write(EncodePktString("unborn HEAD symref-target:refs/heads/" + defaultBranch + "\n"))
				} else if headOid != "" && matchesPrefix("HEAD") {
					line := headOid + " HEAD"
					if symrefs {
						line += " symref-target:refs/heads/" + defaultBranch
					}
					buf.Write(EncodePktString(line + "\n"))
				}

				for refName, oid := range repo.Refs {
					if !matchesPrefix(refName) {
						continue
					}
					line := oid + " " + refName
					if symrefs && refName == "HEAD" {
						line += " symref-target:refs/heads/" + defaultBranch
					}
					if peel && strings.HasPrefix(refName, "refs/tags/") {
						obj, ok := repo.Objects[oid]
						if ok && obj.Type == TypeTag {
							for _, tl := range strings.Split(string(obj.Data), "\n") {
								if strings.HasPrefix(tl, "object ") {
									line += " peeled:" + strings.TrimSpace(tl[7:])
									break
								}
							}
						}
					}
					buf.Write(EncodePktString(line + "\n"))
				}
				buf.Write(PktFlushBytes)

				finalBytes := buf.Bytes()
				repo.LsRefsCache[cacheKey] = finalBytes

				w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
				w.Header().Set("Content-Length", strconv.Itoa(len(finalBytes)))
				w.WriteHeader(http.StatusOK)
				w.Write(finalBytes)
				return
			} else if command == "fetch" {
				var wants []string
				var haves []string
				var clientShallows []string
				depth := -1

				for _, l := range lines {
					if strings.HasPrefix(l, "want ") {
						wants = append(wants, strings.TrimSpace(l[5:]))
					} else if strings.HasPrefix(l, "have ") {
						haves = append(haves, strings.TrimSpace(l[5:]))
					} else if strings.HasPrefix(l, "shallow ") {
						clientShallows = append(clientShallows, strings.TrimSpace(l[8:]))
					} else if strings.HasPrefix(l, "deepen ") {
						d, _ := strconv.Atoi(strings.TrimSpace(l[7:]))
						depth = d
					}
				}

				isShallowClone := depth == 1 && len(wants) == 1 && len(haves) == 0
				shallowCacheKey := ""
				if isShallowClone {
					shallowCacheKey = "shallow1:" + wants[0]
					if cached, ok := repo.ShallowPackCache[shallowCacheKey]; ok {
						w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
						w.Header().Set("Content-Length", strconv.Itoa(len(cached)))
						w.WriteHeader(http.StatusOK)
						w.Write(cached)
						return
					}
				}

				shallowSet := make(map[string]bool)
				for _, s := range clientShallows {
					shallowSet[s] = true
				}
				haveSet := make(map[string]bool)
				for _, h := range haves {
					if shallowSet[h] {
						haveSet[h] = true
						obj, ok := repo.Objects[h]
						if ok && obj.Type == TypeCommit {
							cv, err := ParseCommit(obj.Data)
							if err == nil && cv.Tree != "" {
								for _, toid := range repo.CollectReachableOIDs([]string{cv.Tree}, nil) {
									haveSet[toid] = true
								}
							}
						}
					} else {
						for _, toid := range repo.CollectReachableOIDs([]string{h}, nil) {
							haveSet[toid] = true
						}
					}
				}

				var sendObjects []GitObject
				var shallows []string
				var unshallows []string

				if depth > 0 {
					for _, want := range wants {
						cur := want
						curDepth := 0
						for cur != "" && curDepth < depth {
							obj, ok := repo.Objects[cur]
							if !ok || obj.Type != TypeCommit {
								break
							}
							if !haveSet[cur] {
								sendObjects = append(sendObjects, obj)
								cv, err := ParseCommit(obj.Data)
								if err == nil && cv.Tree != "" {
									for _, toid := range repo.CollectReachableOIDs([]string{cv.Tree}, nil) {
										if !haveSet[toid] {
											if tobj, ok := repo.Objects[toid]; ok {
												sendObjects = append(sendObjects, tobj)
											}
										}
									}
								}
							}
							curDepth++
							cv, err := ParseCommit(obj.Data)
							if err != nil {
								break
							}
							if curDepth == depth {
								if len(cv.Parents) > 0 {
									shallows = append(shallows, cur)
								}
								break
							}
							if len(cv.Parents) == 0 {
								break
							}
							cur = cv.Parents[0]
						}
					}
					for _, cs := range clientShallows {
						csObj, ok := repo.Objects[cs]
						if ok && csObj.Type == TypeCommit {
							cv, err := ParseCommit(csObj.Data)
							if err == nil {
								for _, p := range cv.Parents {
									for _, o := range sendObjects {
										if o.OID == p {
											unshallows = append(unshallows, cs)
											break
										}
									}
								}
							}
						}
					}
				} else {
					toSend := repo.CollectReachableOIDs(wants, haveSet)
					for _, oid := range toSend {
						if obj, ok := repo.Objects[oid]; ok {
							sendObjects = append(sendObjects, obj)
						}
					}
				}

				packfile := WritePack(sendObjects)

				var out bytes.Buffer
				out.Write(EncodePktString("acknowledgments\n"))
				if len(haves) > 0 {
					out.Write(EncodePktString("ready\n"))
				} else {
					out.Write(EncodePktString("NAK\n"))
				}
				out.Write(PktDelimBytes)

				if len(shallows) > 0 || len(unshallows) > 0 {
					out.Write(EncodePktString("shallow-info\n"))
					for _, s := range shallows {
						out.Write(EncodePktString("shallow " + s + "\n"))
					}
					for _, u := range unshallows {
						out.Write(EncodePktString("unshallow " + u + "\n"))
					}
					out.Write(PktDelimBytes)
				}

				out.Write(EncodePktString("packfile\n"))

				// Frame packfile with sideband-64k
				packOffset := 0
				for packOffset < len(packfile) {
					chunkLen := SidebandChunkMax
					if packOffset+chunkLen > len(packfile) {
						chunkLen = len(packfile) - packOffset
					}
					chunk := packfile[packOffset : packOffset+chunkLen]
					out.Write(EncodeSideband1(chunk))
					packOffset += chunkLen
				}
				out.Write(PktFlushBytes)

				finalBytes := out.Bytes()
				if shallowCacheKey != "" {
					repo.ShallowPackCache[shallowCacheKey] = finalBytes
				}

				w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
				w.Header().Set("Content-Length", strconv.Itoa(len(finalBytes)))
				w.WriteHeader(http.StatusOK)
				w.Write(finalBytes)
				return
			}
		} else {
			// v0 upload-pack
			var wants []string
			var haves []string
			depth := -1
			for _, l := range lines {
				if strings.HasPrefix(l, "want ") {
					parts := strings.Fields(l[5:])
					if len(parts) > 0 {
						wants = append(wants, parts[0])
					}
				} else if strings.HasPrefix(l, "have ") {
					haves = append(haves, strings.TrimSpace(l[5:]))
				} else if strings.HasPrefix(l, "deepen ") {
					d, _ := strconv.Atoi(strings.TrimSpace(l[7:]))
					depth = d
				}
			}

			haveSet := make(map[string]bool)
			for _, h := range haves {
				for _, toid := range repo.CollectReachableOIDs([]string{h}, nil) {
					haveSet[toid] = true
				}
			}

			var sendObjects []GitObject
			var shallows []string
			if depth > 0 {
				for _, want := range wants {
					cur := want
					curDepth := 0
					for cur != "" && curDepth < depth {
						obj, ok := repo.Objects[cur]
						if !ok || obj.Type != TypeCommit {
							break
						}
						sendObjects = append(sendObjects, obj)
						cv, err := ParseCommit(obj.Data)
						if err == nil && cv.Tree != "" {
							for _, toid := range repo.CollectReachableOIDs([]string{cv.Tree}, nil) {
								if tobj, ok := repo.Objects[toid]; ok {
									sendObjects = append(sendObjects, tobj)
								}
							}
						}
						curDepth++
						if curDepth == depth {
							for _, p := range cv.Parents {
								shallows = append(shallows, p)
							}
							break
						}
						if len(cv.Parents) == 0 {
							break
						}
						cur = cv.Parents[0]
					}
				}
			} else {
				toSend := repo.CollectReachableOIDs(wants, haveSet)
				for _, oid := range toSend {
					if obj, ok := repo.Objects[oid]; ok {
						sendObjects = append(sendObjects, obj)
					}
				}
			}

			packfile := WritePack(sendObjects)

			var out bytes.Buffer
			if len(shallows) > 0 {
				for _, s := range shallows {
					out.Write(EncodePktString("shallow " + s + "\n"))
				}
				out.Write(PktFlushBytes)
			}
			if len(haves) > 0 {
				out.Write(EncodePktString("ACK\n"))
			} else {
				out.Write(EncodePktString("NAK\n"))
			}

			packOffset := 0
			for packOffset < len(packfile) {
				chunkLen := SidebandChunkMax
				if packOffset+chunkLen > len(packfile) {
					chunkLen = len(packfile) - packOffset
				}
				chunk := packfile[packOffset : packOffset+chunkLen]
				out.Write(EncodeSideband1(chunk))
				packOffset += chunkLen
			}
			out.Write(PktFlushBytes)

			finalBytes := out.Bytes()
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			w.Header().Set("Content-Length", strconv.Itoa(len(finalBytes)))
			w.WriteHeader(http.StatusOK)
			w.Write(finalBytes)
			return
		}
	}

	// POST git-receive-pack
	if gitAction == "git-receive-pack" && r.Method == http.MethodPost {
		bodyBytes, _ := io.ReadAll(r.Body)
		offset := 0
		type cmd struct {
			oldOid string
			newOid string
			ref    string
		}
		var commands []cmd

		for offset < len(bodyBytes) {
			if offset+4 > len(bodyBytes) {
				break
			}
			hexLen := string(bodyBytes[offset : offset+4])
			l, err := strconv.ParseUint(hexLen, 16, 16)
			if err != nil {
				break
			}
			if l == 0 {
				offset += 4
				break // Flush ends commands, packfile follows!
			}
			if l < 4 || int(l) > len(bodyBytes)-offset {
				break
			}
			payload := string(bodyBytes[offset+4 : offset+int(l)])
			offset += int(l)

			line := strings.Split(payload, "\x00")[0]
			parts := strings.Fields(strings.TrimSpace(line))
			if len(parts) >= 3 {
				commands = append(commands, cmd{oldOid: parts[0], newOid: parts[1], ref: parts[2]})
			}
		}

		packBytes := bodyBytes[offset:]
		if len(packBytes) >= 32 && string(packBytes[0:4]) == "PACK" {
			objs, err := ReadPack(packBytes, func(oid string) *GitObject {
				if obj, ok := repo.Objects[oid]; ok {
					return &obj
				}
				return nil
			})
			if err != nil {
				var errReport bytes.Buffer
				errReport.Write(EncodeSideband1String("unpack error: " + err.Error() + "\n"))
				errReport.Write(PktFlushBytes)
				w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
				w.WriteHeader(http.StatusOK)
				w.Write(errReport.Bytes())
				return
			}
			for oid, obj := range objs {
				repo.Objects[oid] = obj
				if obj.Type == TypeCommit {
					cv, err := ParseCommit(obj.Data)
					if err == nil {
						repo.CommitParents[oid] = cv.Parents
					}
				}
			}
		}

		var report bytes.Buffer
		report.Write(EncodeSideband1String("unpack ok\n"))

		for _, c := range commands {
			if c.newOid == ZeroOID {
				delete(repo.Refs, c.ref)
				repo.InvalidateCache()
				report.Write(EncodeSideband1String("ok " + c.ref + "\n"))
			} else if c.oldOid == ZeroOID {
				repo.Refs[c.ref] = c.newOid
				repo.InvalidateCache()
				report.Write(EncodeSideband1String("ok " + c.ref + "\n"))
			} else {
				if repo.IsFastForward(c.oldOid, c.newOid) {
					repo.Refs[c.ref] = c.newOid
					repo.InvalidateCache()
					report.Write(EncodeSideband1String("ok " + c.ref + "\n"))
				} else {
					report.Write(EncodeSideband1String("ng " + c.ref + " non-fast-forward\n"))
				}
			}
		}
		report.Write(PktFlushBytes)

		finalBytes := report.Bytes()
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.Header().Set("Content-Length", strconv.Itoa(len(finalBytes)))
		w.WriteHeader(http.StatusOK)
		w.Write(finalBytes)
		return
	}

	sendJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}, nil)
}
