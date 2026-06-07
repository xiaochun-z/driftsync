export namespace config {
	
	export class LogOptions {
	    ListChecked: boolean;
	    Verbose: boolean;
	
	    static createFrom(source: any = {}) {
	        return new LogOptions(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ListChecked = source["ListChecked"];
	        this.Verbose = source["Verbose"];
	    }
	}
	export class SelectiveYAML {
	    Include: string[];
	    Exclude: string[];
	
	    static createFrom(source: any = {}) {
	        return new SelectiveYAML(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Include = source["Include"];
	        this.Exclude = source["Exclude"];
	    }
	}
	export class Config {
	    Tenant: string;
	    ClientID: string;
	    LocalPath: string;
	    DownloadFromCloud: boolean;
	    UploadFromLocal: boolean;
	    SyncListPath: string;
	    DownloadWorkers: number;
	    UploadWorkers: number;
	    UploadChunkMB: number;
	    UploadParallel: number;
	    Interactive: boolean;
	    Sync?: SelectiveYAML;
	    Log?: LogOptions;
	    is_first_run: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Config(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Tenant = source["Tenant"];
	        this.ClientID = source["ClientID"];
	        this.LocalPath = source["LocalPath"];
	        this.DownloadFromCloud = source["DownloadFromCloud"];
	        this.UploadFromLocal = source["UploadFromLocal"];
	        this.SyncListPath = source["SyncListPath"];
	        this.DownloadWorkers = source["DownloadWorkers"];
	        this.UploadWorkers = source["UploadWorkers"];
	        this.UploadChunkMB = source["UploadChunkMB"];
	        this.UploadParallel = source["UploadParallel"];
	        this.Interactive = source["Interactive"];
	        this.Sync = this.convertValues(source["Sync"], SelectiveYAML);
	        this.Log = this.convertValues(source["Log"], LogOptions);
	        this.is_first_run = source["is_first_run"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	

}

export namespace graph {
	
	export class DriveItemParentReference {
	    path: string;
	
	    static createFrom(source: any = {}) {
	        return new DriveItemParentReference(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	    }
	}
	export class DriveItemFolder {
	    childCount: number;
	
	    static createFrom(source: any = {}) {
	        return new DriveItemFolder(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.childCount = source["childCount"];
	    }
	}
	export class DriveItemHashes {
	    sha256Hash: string;
	
	    static createFrom(source: any = {}) {
	        return new DriveItemHashes(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sha256Hash = source["sha256Hash"];
	    }
	}
	export class DriveItemFile {
	    mimeType: string;
	    hashes?: DriveItemHashes;
	
	    static createFrom(source: any = {}) {
	        return new DriveItemFile(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.mimeType = source["mimeType"];
	        this.hashes = this.convertValues(source["hashes"], DriveItemHashes);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class DriveItem {
	    id: string;
	    name: string;
	    size: number;
	    eTag: string;
	    cTag: string;
	    lastModifiedDateTime: string;
	    file?: DriveItemFile;
	    folder?: DriveItemFolder;
	    parentReference?: DriveItemParentReference;
	    deleted: Record<string, any>;
	
	    static createFrom(source: any = {}) {
	        return new DriveItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.eTag = source["eTag"];
	        this.cTag = source["cTag"];
	        this.lastModifiedDateTime = source["lastModifiedDateTime"];
	        this.file = this.convertValues(source["file"], DriveItemFile);
	        this.folder = this.convertValues(source["folder"], DriveItemFolder);
	        this.parentReference = this.convertValues(source["parentReference"], DriveItemParentReference);
	        this.deleted = source["deleted"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	
	

}

