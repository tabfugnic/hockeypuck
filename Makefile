
_GOPATH=gopath
GOPATH := $(CURDIR)/$(_GOPATH)

GO=go
HOCKEYPATH=$(GOPATH)/src/launchpad.net/hockeypuck

all: get $(HOCKEYPATH)/cmd/hockeypuck/hockeypuck $(HOCKEYPATH)/cmd/hockeypuck-load/hockeypuck-load

get: $(HOCKEYPATH)/mgo

$(HOCKEYPATH)/mgo:
	@echo GOPATH is $(GOPATH)
	mkdir -p $(GOPATH)
	GOPATH="${GOPATH}" $(GO) get launchpad.net/hockeypuck/mgo

$(HOCKEYPATH)/cmd/hockeypuck/hockeypuck:
	cd $(HOCKEYPATH)/cmd/hockeypuck; GOPATH="${GOPATH}" $(GO) build .

$(HOCKEYPATH)/cmd/hockeypuck-load/hockeypuck-load:
	cd $(HOCKEYPATH)/cmd/hockeypuck-load; GOPATH="${GOPATH}" $(GO) build .

debsrc: debbin clean
	debuild -S

debbin:
	debuild -us -uc -i -b

clean:
	$(RM) $(HOCKEYPATH)/cmd/hockeypuck/hockeypuck
	$(RM) $(HOCKEYPATH)/cmd/hockeypuck-load/hockeypuck-load

srcclean:
	$(RM) -r $(GOPATH)