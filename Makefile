install:
	cd $(NAME) && go install -trimpath -buildvcs=false -ldflags "-s -w" .
